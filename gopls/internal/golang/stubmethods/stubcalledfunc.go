// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package stubmethods

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/tools/gopls/internal/util/typesutil"
	"golang.org/x/tools/internal/typesinternal"
)

var anyType = types.Universe.Lookup("any").Type()

// CallStubInfo represents a missing method
// that a receiver type is about to generate
// which has "type X has no field or method Y" error
type CallStubInfo struct {
	Fset       *token.FileSet             // the FileSet used to type-check the types below
	Receiver   typesinternal.NamedOrAlias // the method's receiver type
	MethodName string
	After      types.Object // decl after which to insert the new decl
	pointer    bool
	info       *types.Info
	path       []ast.Node // path enclosing the CallExpr
}

// GetCallStubInfo extracts necessary information to generate a method definition from
// a CallExpr.
func GetCallStubInfo(fset *token.FileSet, info *types.Info, path []ast.Node, pos token.Pos) *CallStubInfo {
	for i, n := range path {
		switch n := n.(type) {
		case *ast.CallExpr:
			s, ok := n.Fun.(*ast.SelectorExpr)
			// TODO: support generating stub functions in the same way.
			if !ok {
				return nil
			}

			// If recvExpr is a package name, compiler error would be
			// e.g., "undefined: http.bar", thus will not hit this code path.
			recvExpr := s.X
			recvType, pointer := concreteType(recvExpr, info)

			if recvType == nil || recvType.Obj().Pkg() == nil {
				return nil
			}

			// A method of a function-local type cannot be stubbed
			// since there's nowhere to put the methods.
			recv := recvType.Obj()
			if recv.Parent() != recv.Pkg().Scope() {
				return nil
			}

			after := types.Object(recv)
			// If the enclosing function declaration is a method declaration,
			// and matches the receiver type of the diagnostic,
			// insert after the enclosing method.
			decl, ok := path[len(path)-2].(*ast.FuncDecl)
			if ok && decl.Recv != nil {
				if len(decl.Recv.List) != 1 {
					return nil
				}
				mrt := info.TypeOf(decl.Recv.List[0].Type)
				if mrt != nil && types.Identical(types.Unalias(typesinternal.Unpointer(mrt)), recv.Type()) {
					after = info.ObjectOf(decl.Name)
				}
			}
			return &CallStubInfo{
				Fset:       fset,
				Receiver:   recvType,
				MethodName: s.Sel.Name,
				After:      after,
				pointer:    pointer,
				path:       path[i:],
				info:       info,
			}
		}
	}
	return nil
}

// Emit writes to out the missing method based on type info of si.Receiver and CallExpr.
func (si *CallStubInfo) Emit(out *bytes.Buffer, qual types.Qualifier) error {
	params := si.collectParams()
	rets := TypesFromContext(si.info, si.path, si.path[0].Pos())
	recv := si.Receiver.Obj()
	// Pointer receiver?
	var star string
	if si.pointer {
		star = "*"
	}

	// Choose receiver name.
	// If any method has a named receiver, choose the first one.
	// Otherwise, use lowercase for the first letter of the object.
	recvName := strings.ToLower(fmt.Sprintf("%.1s", recv.Name()))
	if named, ok := types.Unalias(si.Receiver).(*types.Named); ok {
		for i := 0; i < named.NumMethods(); i++ {
			if recv := named.Method(i).Type().(*types.Signature).Recv(); recv.Name() != "" {
				recvName = recv.Name()
				break
			}
		}
	}

	// Emit method declaration.
	fmt.Fprintf(out, "\nfunc (%s %s%s%s) %s",
		recvName,
		star,
		recv.Name(),
		typesutil.FormatTypeParams(typesinternal.TypeParams(si.Receiver)),
		si.MethodName)

	// Emit parameters, avoiding name conflicts.
	seen := map[string]bool{recvName: true}
	out.WriteString("(")
	for i, param := range params {
		name := param.name
		if seen[name] {
			name = fmt.Sprintf("param%d", i+1)
		}
		seen[name] = true

		if i > 0 {
			out.WriteString(", ")
		}
		fmt.Fprintf(out, "%s %s", name, types.TypeString(param.typ, qual))
	}
	out.WriteString(") ")

	// Emit result types.
	if len(rets) > 1 {
		out.WriteString("(")
	}
	for i, r := range rets {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(types.TypeString(r, qual))
	}
	if len(rets) > 1 {
		out.WriteString(")")
	}

	// Emit body.
	out.WriteString(` {
		panic("unimplemented")
}`)
	return nil
}

type param struct {
	name string
	typ  types.Type // the type of param, inferred from CallExpr
}

// collectParams gathers the parameter information needed to generate a method stub.
// The param's type default to any if there is a type error in the argument.
func (si *CallStubInfo) collectParams() []param {
	var params []param
	appendParam := func(e ast.Expr, t types.Type) {
		p := param{"param", anyType}
		if t != nil && !containsInvalid(t) {
			t = types.Default(t)
			p = param{paramName(e, t), t}
		}
		params = append(params, p)
	}

	args := si.path[0].(*ast.CallExpr).Args
	for _, arg := range args {
		t := si.info.TypeOf(arg)
		switch t := t.(type) {
		// This is the case where another function call returning multiple
		// results is used as an argument.
		case *types.Tuple:
			for ti := 0; ti < t.Len(); ti++ {
				appendParam(arg, t.At(ti).Type())
			}
		default:
			appendParam(arg, t)
		}
	}
	return params
}

// typesFromContext returns the type (or perhaps zero or multiple types)
// of the "hole" into which the expression identified by path must fit.
//
// For example, given
//
//	s, i := "", 0
//	s, i = EXPR
//
// the hole that must be filled by EXPR has type (string, int).
//
// It returns nil on failure.
func TypesFromContext(info *types.Info, path []ast.Node, pos token.Pos) []types.Type {
	var typs []types.Type
	parent := parentNode(path)
	if parent == nil {
		return nil
	}
	switch parent := parent.(type) {
	case *ast.AssignStmt:
		// Append all lhs's type
		if len(parent.Rhs) == 1 {
			for _, lhs := range parent.Lhs {
				t := info.TypeOf(lhs)
				if t != nil && !containsInvalid(t) {
					t = types.Default(t)
				} else {
					t = anyType
				}
				typs = append(typs, t)
			}
			break
		}

		// Lhs and Rhs counts do not match, give up
		if len(parent.Lhs) != len(parent.Rhs) {
			break
		}

		// Append corresponding index of lhs's type
		for i, rhs := range parent.Rhs {
			if rhs.Pos() <= pos && pos <= rhs.End() {
				t := info.TypeOf(parent.Lhs[i])
				if t != nil && !containsInvalid(t) {
					t = types.Default(t)
				} else {
					t = anyType
				}
				typs = append(typs, t)
				break
			}
		}
	case *ast.CallExpr:
		// Find argument containing pos.
		argIdx := -1
		for i, callArg := range parent.Args {
			if callArg.Pos() <= pos && pos <= callArg.End() {
				argIdx = i
				break
			}
		}
		if argIdx == -1 {
			break
		}

		t := info.TypeOf(parent.Fun)
		if t == nil {
			break
		}

		if sig, ok := t.Underlying().(*types.Signature); ok {
			var paramType types.Type
			if sig.Variadic() && argIdx >= sig.Params().Len()-1 {
				v := sig.Params().At(sig.Params().Len() - 1)
				if s, _ := v.Type().(*types.Slice); s != nil {
					paramType = s.Elem()
				}
			} else if argIdx < sig.Params().Len() {
				paramType = sig.Params().At(argIdx).Type()
			} else {
				break
			}
			if paramType == nil || containsInvalid(paramType) {
				paramType = anyType
			}
			typs = append(typs, paramType)
		}
	case *ast.SelectorExpr:
		for _, n := range path {
			assignExpr, ok := n.(*ast.AssignStmt)
			if ok {
				for _, rh := range assignExpr.Rhs {
					// basic types
					basicLit, ok := rh.(*ast.BasicLit)
					if ok {
						switch basicLit.Kind {
						case token.INT:
							typs = append(typs, types.Typ[types.Int])
						case token.FLOAT:
							typs = append(typs, types.Typ[types.Float64])
						case token.IMAG:
							typs = append(typs, types.Typ[types.Complex128])
						case token.STRING:
							typs = append(typs, types.Typ[types.String])
						case token.CHAR:
							typs = append(typs, types.Typ[types.Rune])
						}
						break
					}
					callExpr, ok := rh.(*ast.CallExpr)
					if ok {
						if ident, ok := callExpr.Fun.(*ast.Ident); ok && ident.Name == "make" && len(callExpr.Args) > 0 {
							arg := callExpr.Args[0]
							composite, ok := arg.(*ast.CompositeLit)
							if ok {
								t := typeFromCompositeLit(info, path, composite)
								typs = append(typs, t)
								break
							}
							if t := info.TypeOf(arg); t != nil {
								if !containsInvalid(t) {
									t = types.Default(t)
								} else {
									t = anyType
								}
								typs = append(typs, t)
							}
						}
						if ident, ok := callExpr.Fun.(*ast.Ident); ok && ident.Name == "new" && len(callExpr.Args) > 0 {
							arg := callExpr.Args[0]
							composite, ok := arg.(*ast.CompositeLit)
							if ok {
								t := typeFromCompositeLit(info, path, composite)
								t = types.NewPointer(t)
								typs = append(typs, t)
								break
							}
							if t := info.TypeOf(arg); t != nil {
								if !containsInvalid(t) {
									t = types.Default(t)
									t = types.NewPointer(t)
								} else {
									t = anyType
								}
								typs = append(typs, t)
							}
						}
						break
					}
					// a variable
					ident, ok := rh.(*ast.Ident)
					if ok {
						if t := typeFromIdent(info, path, ident); t != nil {
							typs = append(typs, t)
						}
						break
					}

					selectorExpr, ok := rh.(*ast.SelectorExpr)
					if ok {
						if t := typeFromIdent(info, path, selectorExpr.Sel); t != nil {
							typs = append(typs, t)
						}
						break
					}
					// composite
					composite, ok := rh.(*ast.CompositeLit)
					if ok {
						t := typeFromCompositeLit(info, path, composite)
						typs = append(typs, t)
						break
					}
					// a pointer
					un, ok := rh.(*ast.UnaryExpr)
					if ok && un.Op == token.AND {
						composite, ok := un.X.(*ast.CompositeLit)
						if !ok {
							break
						}
						if t := info.TypeOf(composite); t != nil {
							if !containsInvalid(t) {
								t = types.Default(t)
								t = types.NewPointer(t)
							} else {
								t = anyType
							}
							typs = append(typs, t)
						}
					}
					starExpr, ok := rh.(*ast.StarExpr)
					if ok {
						ident, ok := starExpr.X.(*ast.Ident)
						if ok {
							if t := typeFromIdent(info, path, ident); t != nil {
								if pointer, ok := t.(*types.Pointer); ok {
									t = pointer.Elem()
								}
								typs = append(typs, t)
							}
							break
						}
					}
				}
			}
		}
	default:
		// TODO: support other common kinds of "holes", e.g.
		//   x + EXPR         => typeof(x)
		//   !EXPR            => bool
		//   var x int = EXPR => int
		//   etc.
	}
	return typs
}

// parentNode returns the nodes immediately enclosing path[0],
// ignoring parens.
func parentNode(path []ast.Node) ast.Node {
	if len(path) <= 1 {
		return nil
	}
	for _, n := range path[1:] {
		if _, ok := n.(*ast.ParenExpr); !ok {
			return n
		}
	}
	return nil
}

// containsInvalid checks if the type name contains "invalid type",
// which is not a valid syntax to generate.
func containsInvalid(t types.Type) bool {
	typeString := types.TypeString(t, nil)
	return strings.Contains(typeString, types.Typ[types.Invalid].String())
}

// paramName heuristically chooses a parameter name from
// its argument expression and type. Caller should ensure
// typ is non-nil.
func paramName(e ast.Expr, typ types.Type) string {
	if typ == types.Universe.Lookup("error").Type() {
		return "err"
	}
	switch t := e.(type) {
	// Use the identifier's name as the argument name.
	case *ast.Ident:
		return t.Name
	// Use the Sel.Name's last section as the argument name.
	case *ast.SelectorExpr:
		return lastSection(t.Sel.Name)
	}

	typ = typesinternal.Unpointer(typ)
	switch t := typ.(type) {
	// Use the first character of the type name as the argument name for builtin types
	case *types.Basic:
		return t.Name()[:1]
	case *types.Slice:
		return paramName(e, t.Elem())
	case *types.Array:
		return paramName(e, t.Elem())
	case *types.Signature:
		return "f"
	case *types.Map:
		return "m"
	case *types.Chan:
		return "ch"
	case *types.Named:
		return lastSection(t.Obj().Name())
	default:
		return lastSection(t.String())
	}
}

// lastSection find the position of the last uppercase letter,
// extract the substring from that point onward,
// and convert it to lowercase.
//
// Example: lastSection("registryManagerFactory") = "factory"
func lastSection(identName string) string {
	lastUpperIndex := -1
	for i, r := range identName {
		if unicode.IsUpper(r) {
			lastUpperIndex = i
		}
	}
	if lastUpperIndex != -1 {
		last := identName[lastUpperIndex:]
		return strings.ToLower(last)
	} else {
		return identName
	}
}

func typeFromCompositeLit(info *types.Info, path []ast.Node, composite *ast.CompositeLit) types.Type {
	if t := info.TypeOf(composite); t != nil {
		if !containsInvalid(t) {
			t = types.Default(t)
			if named, ok := t.(*types.Named); ok {
				if pkg := named.Obj().Pkg(); pkg != nil {
					// Find the file in the path that contains this assignment
					var file *ast.File
					for _, n := range path {
						if f, ok := n.(*ast.File); ok {
							file = f
							break
						}
					}

					if file != nil {
						// Look for any import spec that imports this package
						var pkgName string
						for _, imp := range file.Imports {
							if path, _ := strconv.Unquote(imp.Path.Value); path == pkg.Path() {
								// Use the alias if specified, otherwise use package name
								if imp.Name != nil {
									pkgName = imp.Name.Name
								} else {
									pkgName = pkg.Name()
								}
								break
							}
						}
						if pkgName == "" {
							pkgName = pkg.Name() // fallback to package name if no import found
						}

						// Create new package with the correct name (either alias or original)
						newPkg := types.NewPackage(pkgName, pkgName)
						newName := types.NewTypeName(named.Obj().Pos(), newPkg, named.Obj().Name(), nil)
						t = types.NewNamed(newName, named.Underlying(), nil)
					}
				}
				return t
			}
		} else {
			t = anyType
		}
		return t
	}
	return nil
}

func typeFromIdent(info *types.Info, path []ast.Node, ident *ast.Ident) types.Type {
	if t := info.TypeOf(ident); t != nil {
		if !containsInvalid(t) {
			t = types.Default(t)
			if named, ok := t.(*types.Named); ok {
				if pkg := named.Obj().Pkg(); pkg != nil {
					// find the file in the path that contains this assignment
					var file *ast.File
					for _, n := range path {
						if f, ok := n.(*ast.File); ok {
							file = f
							break
						}
					}

					if file != nil {
						// look for any import spec that imports this package
						var pkgName string
						for _, imp := range file.Imports {
							if path, _ := strconv.Unquote(imp.Path.Value); path == pkg.Path() {
								// use the alias if specified, otherwise use package name
								if imp.Name != nil {
									pkgName = imp.Name.Name
								} else {
									pkgName = pkg.Name()
								}
								break
							}
						}
						// fallback to package name if no import found
						if pkgName == "" {
							pkgName = pkg.Name()
						}

						// create new package with the correct name (either alias or original)
						newPkg := types.NewPackage(pkgName, pkgName)
						newName := types.NewTypeName(named.Obj().Pos(), newPkg, named.Obj().Name(), nil)
						t = types.NewNamed(newName, named.Underlying(), nil)
					}
				}
				return t
			}
		} else {
			t = anyType
		}
		return t
	}

	return nil
}
