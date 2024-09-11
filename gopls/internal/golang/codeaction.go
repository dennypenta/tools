// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golang

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/gopls/internal/analysis/fillstruct"
	"golang.org/x/tools/gopls/internal/analysis/fillswitch"
	"golang.org/x/tools/gopls/internal/cache"
	"golang.org/x/tools/gopls/internal/cache/parsego"
	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/label"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/protocol/command"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/typesinternal"
)

// CodeActions returns all enabled code actions (edits and other
// commands) available for the selected range.
//
// Depending on how the request was triggered, fewer actions may be
// offered, e.g. to avoid UI distractions after mere cursor motion.
//
// See ../protocol/codeactionkind.go for some code action theory.
func CodeActions(ctx context.Context, snapshot *cache.Snapshot, fh file.Handle, rng protocol.Range, diagnostics []protocol.Diagnostic, enabled func(protocol.CodeActionKind) bool, trigger protocol.CodeActionTriggerKind) (actions []protocol.CodeAction, _ error) {

	loc := protocol.Location{URI: fh.URI(), Range: rng}

	pgf, err := snapshot.ParseGo(ctx, fh, parsego.Full)
	if err != nil {
		return nil, err
	}
	start, end, err := pgf.RangePos(rng)
	if err != nil {
		return nil, err
	}

	// Scan to see if any enabled producer needs type information.
	var enabledMemo [len(codeActionProducers)]bool
	needTypes := false
	for i, p := range codeActionProducers {
		if enabled(p.kind) {
			enabledMemo[i] = true
			if p.needPkg {
				needTypes = true
			}
		}
	}

	// Compute type information if needed.
	// Also update pgf, start, end to be consistent with pkg.
	// They may differ in case of parse cache miss.
	var pkg *cache.Package
	if needTypes {
		var err error
		pkg, pgf, err = NarrowestPackageForFile(ctx, snapshot, loc.URI)
		if err != nil {
			return nil, err
		}
		start, end, err = pgf.RangePos(loc.Range)
		if err != nil {
			return nil, err
		}
	}

	// Execute each enabled producer function.
	req := &codeActionsRequest{
		actions:     &actions,
		lazy:        make(map[reflect.Type]any),
		snapshot:    snapshot,
		fh:          fh,
		pgf:         pgf,
		loc:         loc,
		start:       start,
		end:         end,
		diagnostics: diagnostics,
		trigger:     trigger,
		pkg:         pkg,
	}
	for i, p := range codeActionProducers {
		if !enabledMemo[i] {
			continue
		}
		req.kind = p.kind
		if p.needPkg {
			req.pkg = pkg
		} else {
			req.pkg = nil
		}
		if err := p.fn(ctx, req); err != nil {
			return nil, err
		}
	}

	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Kind < actions[j].Kind
	})

	return actions, nil
}

// A codeActionsRequest is passed to each function
// that produces code actions.
type codeActionsRequest struct {
	// internal fields for use only by [CodeActions].
	actions *[]protocol.CodeAction // pointer to output slice; call addAction to populate
	lazy    map[reflect.Type]any   // lazy construction

	// inputs to the producer function:
	kind        protocol.CodeActionKind
	snapshot    *cache.Snapshot
	fh          file.Handle
	pgf         *parsego.File
	loc         protocol.Location
	start, end  token.Pos
	diagnostics []protocol.Diagnostic
	trigger     protocol.CodeActionTriggerKind
	pkg         *cache.Package // set only if producer.needPkg
}

// addCommandAction adds a CodeAction to the result based on the provided command.
//
// If the client supports codeAction/resolve, then the command is embedded into
// the code action data field, and used to resolve edits later. Otherwise, the
// command is set as the code action operation.
func (req *codeActionsRequest) addCommandAction(cmd *protocol.Command) *protocol.CodeAction {
	act := protocol.CodeAction{
		Title: cmd.Title,
		Kind:  req.kind,
	}
	// TODO(adonovan): some commands (goFreeSymbols, goplsDoc,
	// goDoc, goTest, and goAssembly) have side effects such as
	// opening a browser, and so should not be eligible for lazy
	// edit resolution.
	if !req.resolveEdits() {
		act.Command = cmd
	} else {
		data, err := json.Marshal(cmd)
		if err != nil {
			panic("unable to marshal")
		}
		msg := json.RawMessage(data)
		act.Data = &msg
	}
	return req.addAction(act)
}

// addCommandAction adds an edit-based CodeAction to the result.
func (req *codeActionsRequest) addEditAction(title string, changes ...protocol.DocumentChange) *protocol.CodeAction {
	return req.addAction(protocol.CodeAction{
		Title: title,
		Kind:  req.kind,
		Edit:  protocol.NewWorkspaceEdit(changes...),
	})
}

// addAction adds a code action to the response.
// It returns an ephemeral pointer to the action in situ.
// which may be used (but only immediately) for further mutation.
func (req *codeActionsRequest) addAction(act protocol.CodeAction) *protocol.CodeAction {
	*req.actions = append(*req.actions, act)
	return &(*req.actions)[len(*req.actions)-1]
}

// resolveEdits reports whether the client can resolve edits lazily.
func (req *codeActionsRequest) resolveEdits() bool {
	opts := req.snapshot.Options()
	return opts.CodeActionResolveOptions != nil &&
		slices.Contains(opts.CodeActionResolveOptions, "edit")
}

// lazyInit[*T](ctx, req) returns a pointer to an instance of T,
// calling new(T).init(ctx.req) on the first request.
//
// It is conceptually a (generic) method of req.
func lazyInit[P interface {
	init(ctx context.Context, req *codeActionsRequest)
	*T
}, T any](ctx context.Context, req *codeActionsRequest) P {
	t := reflect.TypeFor[T]()
	v, ok := req.lazy[t].(P)
	if !ok {
		v = new(T)
		v.init(ctx, req)
		req.lazy[t] = v
	}
	return v
}

// -- producers --

// A codeActionProducer describes a function that produces CodeActions
// of a particular kind.
// The function is only called if that kind is enabled.
type codeActionProducer struct {
	kind    protocol.CodeActionKind
	fn      func(ctx context.Context, req *codeActionsRequest) error
	needPkg bool // fn needs type information (req.pkg)
}

var codeActionProducers = [...]codeActionProducer{
	{kind: protocol.QuickFix, fn: quickFix},
	{kind: protocol.SourceOrganizeImports, fn: sourceOrganizeImports},
	{kind: settings.GoAssembly, fn: goAssembly, needPkg: true},
	{kind: settings.GoDoc, fn: goDoc, needPkg: true},
	{kind: settings.GoFreeSymbols, fn: goFreeSymbols},
	{kind: settings.GoTest, fn: goTest},
	{kind: settings.GoplsDocFeatures, fn: goplsDocFeatures},
	{kind: settings.RefactorExtractFunction, fn: refactorExtractFunction},
	{kind: settings.RefactorExtractMethod, fn: refactorExtractMethod},
	{kind: settings.RefactorExtractToNewFile, fn: refactorExtractToNewFile},
	{kind: settings.RefactorExtractVariable, fn: refactorExtractVariable},
	{kind: settings.RefactorInlineCall, fn: refactorInlineCall, needPkg: true},
	{kind: settings.RefactorRewriteChangeQuote, fn: refactorRewriteChangeQuote},
	{kind: settings.RefactorRewriteFillStruct, fn: refactorRewriteFillStruct, needPkg: true},
	{kind: settings.RefactorRewriteFillSwitch, fn: refactorRewriteFillSwitch, needPkg: true},
	{kind: settings.RefactorRewriteInvertIf, fn: refactorRewriteInvertIf},
	{kind: settings.RefactorRewriteJoinLines, fn: refactorRewriteJoinLines, needPkg: true},
	{kind: settings.RefactorRewriteRemoveUnusedParam, fn: refactorRewriteRemoveUnusedParam, needPkg: true},
	{kind: settings.RefactorRewriteSplitLines, fn: refactorRewriteSplitLines, needPkg: true},

	// Note: don't forget to update the allow-list in Server.CodeAction
	// when adding new query operations like GoTest and GoDoc that
	// are permitted even in generated source files.
}

// sourceOrganizeImports produces "Organize Imports" code actions.
func sourceOrganizeImports(ctx context.Context, req *codeActionsRequest) error {
	res := lazyInit[*allImportsFixesResult](ctx, req)

	// Send all of the import edits as one code action
	// if the file is being organized.
	if len(res.allFixEdits) > 0 {
		req.addEditAction("Organize Imports", protocol.DocumentChangeEdit(req.fh, res.allFixEdits))
	}

	return nil
}

// quickFix produces code actions that add/delete/rename imports to fix type errors.
func quickFix(ctx context.Context, req *codeActionsRequest) error {
	// Only compute quick fixes if there are any diagnostics to fix.
	if len(req.diagnostics) == 0 {
		return nil
	}

	// Process any missing imports and pair them with the diagnostics they fix.
	res := lazyInit[*allImportsFixesResult](ctx, req)
	if res.err != nil {
		return nil
	}

	// Separate this into a set of codeActions per diagnostic, where
	// each action is the addition, removal, or renaming of one import.
	for _, importFix := range res.editsPerFix {
		fixed := fixedByImportFix(importFix.fix, req.diagnostics)
		if len(fixed) == 0 {
			continue
		}
		act := req.addEditAction(
			importFixTitle(importFix.fix),
			protocol.DocumentChangeEdit(req.fh, importFix.edits))
		act.Diagnostics = fixed
	}

	return nil
}

// allImportsFixesResult is the result of a lazy call to allImportsFixes.
// It implements the codeActionsRequest lazyInit interface.
type allImportsFixesResult struct {
	allFixEdits []protocol.TextEdit
	editsPerFix []*importFix
	err         error
}

func (res *allImportsFixesResult) init(ctx context.Context, req *codeActionsRequest) {
	res.allFixEdits, res.editsPerFix, res.err = allImportsFixes(ctx, req.snapshot, req.pgf)
	if res.err != nil {
		event.Error(ctx, "imports fixes", res.err, label.File.Of(req.loc.URI.Path()))
	}
}

func importFixTitle(fix *imports.ImportFix) string {
	var str string
	switch fix.FixType {
	case imports.AddImport:
		str = fmt.Sprintf("Add import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	case imports.DeleteImport:
		str = fmt.Sprintf("Delete import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	case imports.SetImportName:
		str = fmt.Sprintf("Rename import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	}
	return str
}

// fixedByImportFix filters the provided slice of diagnostics to those that
// would be fixed by the provided imports fix.
func fixedByImportFix(fix *imports.ImportFix, diagnostics []protocol.Diagnostic) []protocol.Diagnostic {
	var results []protocol.Diagnostic
	for _, diagnostic := range diagnostics {
		switch {
		// "undeclared name: X" may be an unresolved import.
		case strings.HasPrefix(diagnostic.Message, "undeclared name: "):
			ident := strings.TrimPrefix(diagnostic.Message, "undeclared name: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "undefined: X" may be an unresolved import at Go 1.20+.
		case strings.HasPrefix(diagnostic.Message, "undefined: "):
			ident := strings.TrimPrefix(diagnostic.Message, "undefined: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "could not import: X" may be an invalid import.
		case strings.HasPrefix(diagnostic.Message, "could not import: "):
			ident := strings.TrimPrefix(diagnostic.Message, "could not import: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "X imported but not used" is an unused import.
		// "X imported but not used as Y" is an unused import.
		case strings.Contains(diagnostic.Message, " imported but not used"):
			idx := strings.Index(diagnostic.Message, " imported but not used")
			importPath := diagnostic.Message[:idx]
			if importPath == fmt.Sprintf("%q", fix.StmtInfo.ImportPath) {
				results = append(results, diagnostic)
			}
		}
	}
	return results
}

// goFreeSymbols produces "Browse free symbols" code actions.
// See [server.commandHandler.FreeSymbols] for command implementation.
func goFreeSymbols(ctx context.Context, req *codeActionsRequest) error {
	if !req.loc.Empty() {
		cmd := command.NewFreeSymbolsCommand("Browse free symbols", req.snapshot.View().ID(), req.loc)
		req.addCommandAction(cmd)
	}
	return nil
}

// goplsDocFeatures produces "Browse gopls feature documentation" code actions.
// See [server.commandHandler.ClientOpenURL] for command implementation.
func goplsDocFeatures(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): after the docs are published in gopls/v0.17.0,
	// use the gopls release tag instead of master.
	cmd := command.NewClientOpenURLCommand(
		"Browse gopls feature documentation",
		"https://github.com/golang/tools/blob/master/gopls/doc/features/README.md")
	req.addCommandAction(cmd)
	return nil
}

// goDoc produces "Browse documentation for X" code actions.
// See [server.commandHandler.Doc] for command implementation.
func goDoc(ctx context.Context, req *codeActionsRequest) error {
	_, _, title := DocFragment(req.pkg, req.pgf, req.start, req.end)
	cmd := command.NewDocCommand(title, req.loc)
	req.addCommandAction(cmd)
	return nil
}

// refactorExtractFunction produces "Extract function" code actions.
// See [extractFunction] for command implementation.
func refactorExtractFunction(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, _, _ := canExtractFunction(req.pgf.Tok, req.start, req.end, req.pgf.Src, req.pgf.File); ok {
		cmd := command.NewApplyFixCommand("Extract function", command.ApplyFixArgs{
			Fix:          fixExtractFunction,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorExtractMethod produces "Extract method" code actions.
// See [extractMethod] for command implementation.
func refactorExtractMethod(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, methodOK, _ := canExtractFunction(req.pgf.Tok, req.start, req.end, req.pgf.Src, req.pgf.File); ok && methodOK {
		cmd := command.NewApplyFixCommand("Extract method", command.ApplyFixArgs{
			Fix:          fixExtractMethod,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorExtractVariable produces "Extract variable" code actions.
// See [extractVariable] for command implementation.
func refactorExtractVariable(ctx context.Context, req *codeActionsRequest) error {
	if _, _, ok, _ := canExtractVariable(req.start, req.end, req.pgf.File); ok {
		cmd := command.NewApplyFixCommand("Extract variable", command.ApplyFixArgs{
			Fix:          fixExtractVariable,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorExtractToNewFile produces "Extract declarations to new file" code actions.
// See [server.commandHandler.ExtractToNewFile] for command implementation.
func refactorExtractToNewFile(ctx context.Context, req *codeActionsRequest) error {
	if canExtractToNewFile(req.pgf, req.start, req.end) {
		cmd := command.NewExtractToNewFileCommand("Extract declarations to new file", req.loc)
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorRewriteRemoveUnusedParam produces "Remove unused parameter" code actions.
// See [server.commandHandler.ChangeSignature] for command implementation.
func refactorRewriteRemoveUnusedParam(ctx context.Context, req *codeActionsRequest) error {
	if canRemoveParameter(req.pkg, req.pgf, req.loc.Range) {
		cmd := command.NewChangeSignatureCommand("Refactor: remove unused parameter", command.ChangeSignatureArgs{
			RemoveParameter: req.loc,
			ResolveEdits:    req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorRewriteChangeQuote produces "Convert to {raw,interpreted} string literal" code actions.
func refactorRewriteChangeQuote(ctx context.Context, req *codeActionsRequest) error {
	convertStringLiteral(req)
	return nil
}

// refactorRewriteChangeQuote produces "Invert 'if' condition" code actions.
// See [invertIfCondition] for command implementation.
func refactorRewriteInvertIf(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, _ := canInvertIfCondition(req.pgf.File, req.start, req.end); ok {
		cmd := command.NewApplyFixCommand("Invert 'if' condition", command.ApplyFixArgs{
			Fix:          fixInvertIfCondition,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorRewriteSplitLines produces "Split ITEMS into separate lines" code actions.
// See [splitLines] for command implementation.
func refactorRewriteSplitLines(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): opt: don't set needPkg just for FileSet.
	if msg, ok, _ := canSplitLines(req.pgf.File, req.pkg.FileSet(), req.start, req.end); ok {
		cmd := command.NewApplyFixCommand(msg, command.ApplyFixArgs{
			Fix:          fixSplitLines,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorRewriteJoinLines produces "Join ITEMS into one line" code actions.
// See [joinLines] for command implementation.
func refactorRewriteJoinLines(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): opt: don't set needPkg just for FileSet.
	if msg, ok, _ := canJoinLines(req.pgf.File, req.pkg.FileSet(), req.start, req.end); ok {
		cmd := command.NewApplyFixCommand(msg, command.ApplyFixArgs{
			Fix:          fixJoinLines,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// refactorRewriteFillStruct produces "Join ITEMS into one line" code actions.
// See [fillstruct.SuggestedFix] for command implementation.
func refactorRewriteFillStruct(ctx context.Context, req *codeActionsRequest) error {
	// fillstruct.Diagnose is a lazy analyzer: all it gives us is
	// the (start, end, message) of each SuggestedFix; the actual
	// edit is computed only later by ApplyFix, which calls fillstruct.SuggestedFix.
	for _, diag := range fillstruct.Diagnose(req.pgf.File, req.start, req.end, req.pkg.Types(), req.pkg.TypesInfo()) {
		rng, err := req.pgf.Mapper.PosRange(req.pgf.Tok, diag.Pos, diag.End)
		if err != nil {
			return err
		}
		for _, fix := range diag.SuggestedFixes {
			cmd := command.NewApplyFixCommand(fix.Message, command.ApplyFixArgs{
				Fix:          diag.Category,
				URI:          req.loc.URI,
				Range:        rng,
				ResolveEdits: req.resolveEdits(),
			})
			req.addCommandAction(cmd)
		}
	}
	return nil
}

// refactorRewriteFillSwitch produces "Add cases for TYPE/ENUM" code actions.
func refactorRewriteFillSwitch(ctx context.Context, req *codeActionsRequest) error {
	for _, diag := range fillswitch.Diagnose(req.pgf.File, req.start, req.end, req.pkg.Types(), req.pkg.TypesInfo()) {
		changes, err := suggestedFixToDocumentChange(ctx, req.snapshot, req.pkg.FileSet(), &diag.SuggestedFixes[0])
		if err != nil {
			return err
		}
		req.addEditAction(diag.Message, changes...)
	}

	return nil
}

// canRemoveParameter reports whether we can remove the function parameter
// indicated by the given [start, end) range.
//
// This is true if:
//   - there are no parse or type errors, and
//   - [start, end) is contained within an unused field or parameter name
//   - ... of a non-method function declaration.
//
// (Note that the unusedparam analyzer also computes this property, but
// much more precisely, allowing it to report its findings as diagnostics.)
//
// TODO(adonovan): inline into refactorRewriteRemoveUnusedParam.
func canRemoveParameter(pkg *cache.Package, pgf *parsego.File, rng protocol.Range) bool {
	if perrors, terrors := pkg.ParseErrors(), pkg.TypeErrors(); len(perrors) > 0 || len(terrors) > 0 {
		return false // can't remove parameters from packages with errors
	}
	info, err := findParam(pgf, rng)
	if err != nil {
		return false // e.g. invalid range
	}
	if info.field == nil {
		return false // range does not span a parameter
	}
	if info.decl.Body == nil {
		return false // external function
	}
	if len(info.field.Names) == 0 {
		return true // no names => field is unused
	}
	if info.name == nil {
		return false // no name is indicated
	}
	if info.name.Name == "_" {
		return true // trivially unused
	}

	obj := pkg.TypesInfo().Defs[info.name]
	if obj == nil {
		return false // something went wrong
	}

	used := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if n, ok := node.(*ast.Ident); ok && pkg.TypesInfo().Uses[n] == obj {
			used = true
		}
		return !used // keep going until we find a use
	})
	return !used
}

// refactorInlineCall produces "Inline call to FUNC" code actions.
// See [inlineCall] for command implementation.
func refactorInlineCall(ctx context.Context, req *codeActionsRequest) error {
	// To avoid distraction (e.g. VS Code lightbulb), offer "inline"
	// only after a selection or explicit menu operation.
	if req.trigger == protocol.CodeActionAutomatic && req.loc.Empty() {
		return nil
	}

	// If range is within call expression, offer to inline the call.
	if _, fn, err := enclosingStaticCall(req.pkg, req.pgf, req.start, req.end); err == nil {
		cmd := command.NewApplyFixCommand(fmt.Sprintf("Inline call to %s", fn.Name()), command.ApplyFixArgs{
			Fix:          fixInlineCall,
			URI:          req.loc.URI,
			Range:        req.loc.Range,
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd)
	}
	return nil
}

// goTest produces "Run tests and benchmarks" code actions.
// See [server.commandHandler.runTests] for command implementation.
func goTest(ctx context.Context, req *codeActionsRequest) error {
	testFuncs, benchFuncs, err := testsAndBenchmarks(req.pkg.TypesInfo(), req.pgf)
	if err != nil {
		return err
	}

	var tests, benchmarks []string
	for _, fn := range testFuncs {
		if protocol.Intersect(fn.rng, req.loc.Range) {
			tests = append(tests, fn.name)
		}
	}
	for _, fn := range benchFuncs {
		if protocol.Intersect(fn.rng, req.loc.Range) {
			benchmarks = append(benchmarks, fn.name)
		}
	}

	if len(tests) == 0 && len(benchmarks) == 0 {
		return nil
	}

	cmd := command.NewTestCommand("Run tests and benchmarks", req.loc.URI, tests, benchmarks)
	req.addCommandAction(cmd)
	return nil
}

// goAssembly produces "Browse ARCH assembly for FUNC" code actions.
// See [server.commandHandler.Assembly] for command implementation.
func goAssembly(ctx context.Context, req *codeActionsRequest) error {
	view := req.snapshot.View()

	// Find the enclosing toplevel function or method,
	// and compute its symbol name (e.g. "pkgpath.(T).method").
	// The report will show this method and all its nested
	// functions (FuncLit, defers, etc).
	//
	// TODO(adonovan): this is no good for generics, since they
	// will always be uninstantiated when they enclose the cursor.
	// Instead, we need to query the func symbol under the cursor,
	// rather than the enclosing function. It may be an explicitly
	// or implicitly instantiated generic, and it may be defined
	// in another package, though we would still need to compile
	// the current package to see its assembly. The challenge,
	// however, is that computing the linker name for a generic
	// symbol is quite tricky. Talk with the compiler team for
	// ideas.
	//
	// TODO(adonovan): think about a smoother UX for jumping
	// directly to (say) a lambda of interest.
	// Perhaps we could scroll to STEXT for the innermost
	// enclosing nested function?
	path, _ := astutil.PathEnclosingInterval(req.pgf.File, req.start, req.end)
	if len(path) >= 2 { // [... FuncDecl File]
		if decl, ok := path[len(path)-2].(*ast.FuncDecl); ok {
			if fn, ok := req.pkg.TypesInfo().Defs[decl.Name].(*types.Func); ok {
				sig := fn.Signature()

				// Compute the linker symbol of the enclosing function.
				var sym strings.Builder
				if fn.Pkg().Name() == "main" {
					sym.WriteString("main")
				} else {
					sym.WriteString(fn.Pkg().Path())
				}
				sym.WriteString(".")
				if sig.Recv() != nil {
					if isPtr, named := typesinternal.ReceiverNamed(sig.Recv()); named != nil {
						sym.WriteString("(")
						if isPtr {
							sym.WriteString("*")
						}
						sym.WriteString(named.Obj().Name())
						sym.WriteString(").")
					}
				}
				sym.WriteString(fn.Name())

				if fn.Name() != "_" && // blank functions are not compiled
					(fn.Name() != "init" || sig.Recv() != nil) && // init functions aren't linker functions
					sig.TypeParams() == nil && sig.RecvTypeParams() == nil { // generic => no assembly
					cmd := command.NewAssemblyCommand(
						fmt.Sprintf("Browse %s assembly for %s", view.GOARCH(), decl.Name),
						view.ID(),
						string(req.pkg.Metadata().ID),
						sym.String())
					req.addCommandAction(cmd)
				}
			}
		}
	}
	return nil
}
