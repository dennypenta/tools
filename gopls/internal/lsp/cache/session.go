// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/lsp/cache/metadata"
	"golang.org/x/tools/gopls/internal/lsp/cache/typerefs"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/util/bug"
	"golang.org/x/tools/gopls/internal/util/persistent"
	"golang.org/x/tools/gopls/internal/vulncheck"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/memoize"
	"golang.org/x/tools/internal/xcontext"
)

type Session struct {
	// Unique identifier for this session.
	id string

	// Immutable attributes shared across views.
	cache       *Cache            // shared cache
	gocmdRunner *gocommand.Runner // limits go command concurrency

	viewMu  sync.Mutex
	views   []*View
	viewMap map[protocol.DocumentURI]*View // file->best view; nil after shutdown

	parseCache *parseCache

	*overlayFS
}

// ID returns the unique identifier for this session on this server.
func (s *Session) ID() string     { return s.id }
func (s *Session) String() string { return s.id }

// GoCommandRunner returns the gocommand Runner for this session.
func (s *Session) GoCommandRunner() *gocommand.Runner {
	return s.gocmdRunner
}

// Shutdown the session and all views it has created.
func (s *Session) Shutdown(ctx context.Context) {
	var views []*View
	s.viewMu.Lock()
	views = append(views, s.views...)
	s.views = nil
	s.viewMap = nil
	s.viewMu.Unlock()
	for _, view := range views {
		view.shutdown()
	}
	s.parseCache.stop()
	event.Log(ctx, "Shutdown session", KeyShutdownSession.Of(s))
}

// Cache returns the cache that created this session, for debugging only.
func (s *Session) Cache() *Cache {
	return s.cache
}

// TODO(rfindley): is the logic surrounding this error actually necessary?
var ErrViewExists = errors.New("view already exists for session")

// NewView creates a new View, returning it and its first snapshot. If a
// non-empty tempWorkspace directory is provided, the View will record a copy
// of its gopls workspace module in that directory, so that client tooling
// can execute in the same main module.  On success it also returns a release
// function that must be called when the Snapshot is no longer needed.
func (s *Session) NewView(ctx context.Context, folder *Folder) (*View, *Snapshot, func(), error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()

	// Querying the file system to check whether
	// two folders denote the same existing directory.
	if inode1, err := os.Stat(filepath.FromSlash(folder.Dir.Path())); err == nil {
		for _, view := range s.views {
			inode2, err := os.Stat(filepath.FromSlash(view.folder.Dir.Path()))
			if err == nil && os.SameFile(inode1, inode2) {
				return nil, nil, nil, ErrViewExists
			}
		}
	}

	def, err := defineView(ctx, s, folder, "")
	if err != nil {
		return nil, nil, nil, err
	}
	view, snapshot, release := s.createView(ctx, def, folder)
	s.views = append(s.views, view)
	// we always need to drop the view map
	s.viewMap = make(map[protocol.DocumentURI]*View)
	return view, snapshot, release, nil
}

// createView creates a new view, with an initial snapshot that retains the
// supplied context, detached from events and cancelation.
//
// The caller is responsible for calling the release function once.
func (s *Session) createView(ctx context.Context, def *viewDefinition, folder *Folder) (*View, *Snapshot, func()) {
	index := atomic.AddInt64(&viewIndex, 1)

	// We want a true background context and not a detached context here
	// the spans need to be unrelated and no tag values should pollute it.
	baseCtx := event.Detach(xcontext.Detach(ctx))
	backgroundCtx, cancel := context.WithCancel(baseCtx)

	// Compute a skip function to use for module cache scanning.
	//
	// Note that unlike other filtering operations, we definitely don't want to
	// exclude the gomodcache here, even if it is contained in the workspace
	// folder.
	//
	// TODO(rfindley): consolidate with relPathExcludedByFilter(Func), Filterer,
	// View.filterFunc.
	var skipPath func(string) bool
	{
		// Compute a prefix match, respecting segment boundaries, by ensuring
		// the pattern (dir) has a trailing slash.
		dirPrefix := strings.TrimSuffix(string(folder.Dir), "/") + "/"
		filterer := NewFilterer(folder.Options.DirectoryFilters)
		skipPath = func(dir string) bool {
			uri := strings.TrimSuffix(string(protocol.URIFromPath(dir)), "/")
			// Note that the logic below doesn't handle the case where uri ==
			// v.folder.Dir, because there is no point in excluding the entire
			// workspace folder!
			if rel := strings.TrimPrefix(uri, dirPrefix); rel != uri {
				return filterer.Disallow(rel)
			}
			return false
		}
	}

	var ignoreFilter *ignoreFilter
	{
		var dirs []string
		if len(def.workspaceModFiles) == 0 {
			for _, entry := range filepath.SplitList(def.gopath) {
				dirs = append(dirs, filepath.Join(entry, "src"))
			}
		} else {
			dirs = append(dirs, def.gomodcache)
			for m := range def.workspaceModFiles {
				dirs = append(dirs, filepath.Dir(m.Path()))
			}
		}
		ignoreFilter = newIgnoreFilter(dirs)
	}

	v := &View{
		id:                   strconv.FormatInt(index, 10),
		gocmdRunner:          s.gocmdRunner,
		folder:               folder,
		initialWorkspaceLoad: make(chan struct{}),
		initializationSema:   make(chan struct{}, 1),
		baseCtx:              baseCtx,
		parseCache:           s.parseCache,
		ignoreFilter:         ignoreFilter,
		fs:                   s.overlayFS,
		viewDefinition:       def,
		importsState: &importsState{
			ctx: backgroundCtx,
			processEnv: &imports.ProcessEnv{
				GocmdRunner:    s.gocmdRunner,
				SkipPathInScan: skipPath,
			},
		},
	}

	v.snapshot = &Snapshot{
		view:             v,
		backgroundCtx:    backgroundCtx,
		cancel:           cancel,
		store:            s.cache.store,
		refcount:         1, // Snapshots are born referenced.
		packages:         new(persistent.Map[PackageID, *packageHandle]),
		meta:             new(metadata.Graph),
		files:            newFileMap(),
		activePackages:   new(persistent.Map[PackageID, *Package]),
		symbolizeHandles: new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		shouldLoad:       new(persistent.Map[PackageID, []PackagePath]),
		unloadableFiles:  new(persistent.Set[protocol.DocumentURI]),
		parseModHandles:  new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		parseWorkHandles: new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		modTidyHandles:   new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		modVulnHandles:   new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		modWhyHandles:    new(persistent.Map[protocol.DocumentURI, *memoize.Promise]),
		pkgIndex:         typerefs.NewPackageIndex(),
		moduleUpgrades:   new(persistent.Map[protocol.DocumentURI, map[string]string]),
		vulns:            new(persistent.Map[protocol.DocumentURI, *vulncheck.Result]),
	}

	// Record the environment of the newly created view in the log.
	event.Log(ctx, viewEnv(v))

	// Initialize the view without blocking.
	initCtx, initCancel := context.WithCancel(xcontext.Detach(ctx))
	v.initCancelFirstAttempt = initCancel
	snapshot := v.snapshot

	// Pass a second reference to the background goroutine.
	bgRelease := snapshot.Acquire()
	go func() {
		defer bgRelease()
		snapshot.initialize(initCtx, true)
	}()

	// Return a third reference to the caller.
	return v, snapshot, snapshot.Acquire()
}

// RemoveView removes from the session the view rooted at the specified directory.
// It reports whether a view of that directory was removed.
func (s *Session) RemoveView(dir protocol.DocumentURI) bool {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	for _, view := range s.views {
		if view.folder.Dir == dir {
			i := s.dropView(view)
			if i == -1 {
				return false // can't happen
			}
			// delete this view... we don't care about order but we do want to make
			// sure we can garbage collect the view
			s.views = removeElement(s.views, i)
			return true
		}
	}
	return false
}

// View returns the view with a matching id, if present.
func (s *Session) View(id string) (*View, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	for _, view := range s.views {
		if view.ID() == id {
			return view, nil
		}
	}
	return nil, fmt.Errorf("no view with ID %q", id)
}

// SnapshotOf returns a Snapshot corresponding to the given URI.
//
// In the case where the file can be  can be associated with a View by
// bestViewForURI (based on directory information alone, without package
// metadata), SnapshotOf returns the current Snapshot for that View. Otherwise,
// it awaits loading package metadata and returns a Snapshot for the first View
// containing a real (=not command-line-arguments) package for the file.
//
// If that also fails to find a View, SnapshotOf returns a Snapshot for the
// first view in s.views that is not shut down (i.e. s.views[0] unless we lose
// a race), for determinism in tests and so that we tend to aggregate the
// resulting command-line-arguments packages into a single view.
//
// SnapshotOf returns an error if a failure occurs along the way (most likely due
// to context cancellation), or if there are no Views in the Session.
func (s *Session) SnapshotOf(ctx context.Context, uri protocol.DocumentURI) (*Snapshot, func(), error) {
	// Fast path: if the uri has a static association with a view, return it.
	s.viewMu.Lock()
	v, err := s.viewOfLocked(ctx, uri)
	s.viewMu.Unlock()

	if err != nil {
		return nil, nil, err
	}

	if v != nil {
		snapshot, release, err := v.Snapshot()
		if err == nil {
			return snapshot, release, nil
		}
		// View is shut down. Forget this association.
		s.viewMu.Lock()
		if s.viewMap[uri] == v {
			delete(s.viewMap, uri)
		}
		s.viewMu.Unlock()
	}

	// Fall-back: none of the views could be associated with uri based on
	// directory information alone.
	//
	// Don't memoize the view association in viewMap, as it is not static: Views
	// may change as metadata changes.
	//
	// TODO(rfindley): we could perhaps optimize this case by peeking at existing
	// metadata before awaiting the load (after all, a load only adds metadata).
	// But that seems potentially tricky, when in the common case no loading
	// should be required.
	views := s.Views()
	for _, v := range views {
		snapshot, release, err := v.Snapshot()
		if err != nil {
			continue // view was shut down
		}
		_ = snapshot.awaitLoaded(ctx) // ignore error
		g := snapshot.MetadataGraph()
		// We don't check the error from awaitLoaded, because a load failure (that
		// doesn't result from context cancelation) should not prevent us from
		// continuing to search for the best view.
		if ctx.Err() != nil {
			release()
			return nil, nil, ctx.Err()
		}
		// Special handling for the builtin file, since it doesn't have packages.
		if snapshot.IsBuiltin(uri) {
			return snapshot, release, nil
		}
		// Only match this view if it loaded a real package for the file.
		//
		// Any view can load a command-line-arguments package; aggregate those into
		// views[0] below.
		for _, id := range g.IDs[uri] {
			if !metadata.IsCommandLineArguments(id) || g.Packages[id].Standalone {
				return snapshot, release, nil
			}
		}
		release()
	}

	for _, v := range views {
		snapshot, release, err := v.Snapshot()
		if err == nil {
			return snapshot, release, nil // first valid snapshot
		}
	}
	return nil, nil, errNoViews
}

// errNoViews is sought by orphaned file diagnostics, to detect the case where
// we have no view containing a file.
var errNoViews = errors.New("no views")

// viewOfLocked wraps bestViewForURI, memoizing its result.
//
// Precondition: caller holds s.viewMu lock.
//
// May return (nil, nil).
func (s *Session) viewOfLocked(ctx context.Context, uri protocol.DocumentURI) (*View, error) {
	v, hit := s.viewMap[uri]
	if !hit {
		// Cache miss: compute (and memoize) the best view.
		var defs []*viewDefinition
		viewLookup := make(map[*viewDefinition]*View)
		for _, v := range s.views {
			defs = append(defs, v.viewDefinition)
			viewLookup[v.viewDefinition] = v
		}
		def, err := bestViewDefForURI(ctx, s, uri, defs)
		if err != nil {
			return nil, err
		}
		v = viewLookup[def] // possibly nil
		if s.viewMap == nil {
			return nil, errors.New("session is shut down")
		}
		s.viewMap[uri] = v
	}
	return v, nil
}

func (s *Session) Views() []*View {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	result := make([]*View, len(s.views))
	copy(result, s.views)
	return result
}

// selectViews constructs the best set of views covering the provided workspace
// folders and open files.
//
// This implements the zero-config algorithm of golang/go#57979.
func selectViews(ctx context.Context, fs file.Source, folders []*Folder, openFiles []protocol.DocumentURI) ([]*viewDefinition, error) {
	var views []*viewDefinition

	// First, compute a default view for each workspace folder.
	// TODO(golang/go#57979): technically, this is path dependent, since
	// DidChangeWorkspaceFolders could introduce a path-dependent ordering on
	// folders. We should keep folders sorted, or sort them here.
	for _, folder := range folders {
		view, err := defineView(ctx, fs, folder, "")
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}

	// Next, ensure that the set of views covers all open files contained in a
	// workspace folder.
	//
	// We only do this for files contained in a workspace folder, because other
	// open files are most likely the result of jumping to a definition from a
	// workspace file; we don't want to create additional views in those cases:
	// they should be resolved after initialization.

	folderForFile := func(uri protocol.DocumentURI) *Folder {
		var longest *Folder
		for _, folder := range folders {
			if (longest == nil || len(folder.Dir) > len(longest.Dir)) && folder.Dir.Encloses(uri) {
				longest = folder
			}
		}
		return longest
	}

checkFiles:
	for _, uri := range openFiles {
		folder := folderForFile(uri)
		if folder == nil {
			continue // only guess views for open files
		}
		view, err := bestViewDefForURI(ctx, fs, uri, views)
		if err != nil {
			return nil, err
		}
		if view != nil {
			continue // file covered by an existing view
		}
		view, err = defineView(ctx, fs, folder, uri)
		if err != nil {
			return nil, err
		}
		// It need not strictly be the case that the best view for a file is
		// distinct from other views, as the logic of getViewDefinition and
		// bestViewForURI does not align perfectly. This is not necessarily a bug:
		// there may be files for which we can't construct a valid view.
		//
		// Nevertheless, we should not create redundant views.
		for _, alt := range views {
			if viewDefinitionsEqual(alt, view) {
				continue checkFiles
			}
		}
		views = append(views, view)
	}

	return views, nil
}

// bestViewDefForURI returns the existing view that contains the existing file,
// or (nil, nil) if no matching view is found.
//
// The provided uri must be a file uri, not directory.
//
// bestViewDefForURI only returns an error in the event of context cancellation.
//
// TODO(rfindley): if we pass a file's []constraint.Expr here, we can implement
// improved build tag support.
func bestViewDefForURI(ctx context.Context, fs file.Source, uri protocol.DocumentURI, views []*viewDefinition) (*viewDefinition, error) {
	if len(views) == 0 {
		return nil, nil // avoid the call to findRootPattern
	}
	dir := uri.Dir()
	modURI, err := findRootPattern(ctx, dir, "go.mod", fs)
	if err != nil {
		return nil, err
	}

	// Prefer GoWork > GoMod > GOPATH > GoPackages > AdHoc.
	var (
		goPackagesView *viewDefinition // prefer longest
		gopathView     *viewDefinition // prefer longest
		adHocView      *viewDefinition // exact match
		modView        *viewDefinition // exact match
		// TODO(rfindley): should we also prefer the longest matching go.work view?
		// If two go.work files contain a module, which one is more natural to use?
	)
	for _, view := range views {
		switch view.Type() {
		case GoPackagesDriverView:
			if goPackagesView != nil && len(goPackagesView.root) > len(view.root) {
				continue
			}
			if view.root.Encloses(dir) {
				goPackagesView = view
			}
		case GoWorkView:
			if uri == view.gowork {
				return view, nil
			}
			if _, ok := view.workspaceModFiles[modURI]; ok {
				return view, nil
			}
		case GoModView:
			if modURI == view.gomod {
				modView = view
			}
		case GOPATHView:
			if gopathView != nil && len(gopathView.root) > len(view.root) {
				continue
			}
			if view.root.Encloses(dir) {
				gopathView = view
			}
		case AdHocView:
			if view.root == dir {
				adHocView = view
			}
		}
	}
	if modView != nil {
		return modView, nil
	}
	if gopathView != nil {
		return gopathView, nil
	}
	if goPackagesView != nil {
		return goPackagesView, nil
	}
	if adHocView != nil {
		return adHocView, nil
	}
	return nil, nil // no view found
}

// updateViewLocked recreates the view with the given options.
//
// If the resulting error is non-nil, the view may or may not have already been
// dropped from the session.
func (s *Session) updateViewLocked(ctx context.Context, view *View, def *viewDefinition, folder *Folder) (*View, error) {
	i := s.dropView(view)
	if i == -1 {
		return nil, fmt.Errorf("view %q not found", view.id)
	}

	view, snapshot, release := s.createView(ctx, def, folder)
	defer release()

	// The new snapshot has lost the history of the previous view. As a result,
	// it may not see open files that aren't in its build configuration (as it
	// would have done via didOpen notifications). This can lead to inconsistent
	// behavior when configuration is changed mid-session.
	//
	// Ensure the new snapshot observes all open files.
	for _, o := range view.fs.Overlays() {
		_, _ = snapshot.ReadFile(ctx, o.URI())
	}

	// substitute the new view into the array where the old view was
	s.views[i] = view
	s.viewMap = make(map[protocol.DocumentURI]*View)
	return view, nil
}

// removeElement removes the ith element from the slice replacing it with the last element.
// TODO(adonovan): generics, someday.
func removeElement(slice []*View, index int) []*View {
	last := len(slice) - 1
	slice[index] = slice[last]
	slice[last] = nil // aid GC
	return slice[:last]
}

// dropView removes v from the set of views for the receiver s and calls
// v.shutdown, returning the index of v in s.views (if found), or -1 if v was
// not found. s.viewMu must be held while calling this function.
func (s *Session) dropView(v *View) int {
	// we always need to drop the view map
	s.viewMap = make(map[protocol.DocumentURI]*View)
	for i := range s.views {
		if v == s.views[i] {
			// we found the view, drop it and return the index it was found at
			s.views[i] = nil
			v.shutdown()
			return i
		}
	}
	// TODO(rfindley): it looks wrong that we don't shutdown v in this codepath.
	// We should never get here.
	bug.Reportf("tried to drop nonexistent view %q", v.id)
	return -1
}

// ResetView resets the best view for the given URI.
func (s *Session) ResetView(ctx context.Context, uri protocol.DocumentURI) (*View, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	v, err := s.viewOfLocked(ctx, uri)
	if err != nil {
		return nil, err
	}
	return s.updateViewLocked(ctx, v, v.viewDefinition, v.folder)
}

// DidModifyFiles reports a file modification to the session. It returns
// the new snapshots after the modifications have been applied, paired with
// the affected file URIs for those snapshots.
// On success, it returns a release function that
// must be called when the snapshots are no longer needed.
//
// TODO(rfindley): what happens if this function fails? It must leave us in a
// broken state, which we should surface to the user, probably as a request to
// restart gopls.
func (s *Session) DidModifyFiles(ctx context.Context, changes []file.Modification) (map[*View][]protocol.DocumentURI, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()

	// Update overlays.
	//
	// This is done while holding viewMu because the set of open files affects
	// the set of views, and to prevent views from seeing updated file content
	// before they have processed invalidations.
	if err := s.updateOverlays(ctx, changes); err != nil {
		return nil, err
	}

	// checkViews controls whether the set of views needs to be recomputed, for
	// example because a go.mod file was created or deleted, or a go.work file
	// changed on disk.
	checkViews := false

	changed := make(map[protocol.DocumentURI]file.Handle)
	for _, c := range changes {
		changed[c.URI] = mustReadFile(ctx, s, c.URI)
		// Any on-disk change to a go.work file causes a re-diagnosis.
		//
		// TODO(rfindley): go.work files need not be named "go.work" -- we need to
		// check each view's source to handle the case of an explicit GOWORK value.
		// Write a test that fails, and fix this.
		if isGoWork(c.URI) && (c.Action == file.Save || c.OnDisk) {
			checkViews = true
		}
		// Opening/Close/Create/Delete of go.mod files all trigger
		// re-evaluation of Views. Changes do not as they can't affect the set of
		// Views.
		if isGoMod(c.URI) && c.Action != file.Change && c.Action != file.Save {
			checkViews = true
		}
	}

	if checkViews {
		for _, view := range s.views {
			// TODO(rfindley): can we avoid running the go command (go env)
			// synchronously to change processing? Can we assume that the env did not
			// change, and derive go.work using a combination of the configured
			// GOWORK value and filesystem?
			info, err := defineView(ctx, s, view.folder, "")
			if err != nil {
				// Catastrophic failure, equivalent to a failure of session
				// initialization and therefore should almost never happen. One
				// scenario where this failure mode could occur is if some file
				// permissions have changed preventing us from reading go.mod
				// files.
				//
				// TODO(rfindley): consider surfacing this error more loudly. We
				// could report a bug, but it's not really a bug.
				event.Error(ctx, "fetching workspace information", err)
			} else if !viewDefinitionsEqual(view.viewDefinition, info) {
				if _, err := s.updateViewLocked(ctx, view, info, view.folder); err != nil {
					// More catastrophic failure. The view may or may not still exist.
					// The best we can do is log and move on.
					event.Error(ctx, "recreating view", err)
				}
			}
		}
	}

	// Collect view definitions, for resolving the best view for each change.
	var viewDefinitions []*viewDefinition
	viewLookup := make(map[*viewDefinition]*View)
	for _, view := range s.views {
		viewDefinitions = append(viewDefinitions, view.viewDefinition)
		viewLookup[view.viewDefinition] = view
	}

	// We only want to run fast-path diagnostics (i.e. diagnoseChangedFiles) once
	// for each changed file, in its best view. Collect files into their best
	// views.
	viewsToDiagnose := make(map[*View][]protocol.DocumentURI)
	for _, mod := range changes {
		def, err := bestViewDefForURI(ctx, s, mod.URI, viewDefinitions)
		if err != nil {
			// bestViewForURI only returns an error in the event of context
			// cancellation. Since state changes should occur on an uncancellable
			// context, an error here is a bug.
			bug.Reportf("finding best view for change: %v", err)
			continue
		}
		if def != nil {
			v := viewLookup[def]
			viewsToDiagnose[v] = append(viewsToDiagnose[v], mod.URI)
		}
	}

	// ...but changes may be relevant to other views, for example if they are
	// changes to a shared packaged.
	for _, v := range s.views {
		_, release, needsDiagnosis := v.Invalidate(ctx, StateChange{Files: changed})
		release()

		if needsDiagnosis || checkViews {
			if _, ok := viewsToDiagnose[v]; !ok {
				viewsToDiagnose[v] = nil
			}
		}
	}

	return viewsToDiagnose, nil
}

// ExpandModificationsToDirectories returns the set of changes with the
// directory changes removed and expanded to include all of the files in
// the directory.
func (s *Session) ExpandModificationsToDirectories(ctx context.Context, changes []file.Modification) []file.Modification {
	var snapshots []*Snapshot
	s.viewMu.Lock()
	for _, v := range s.views {
		snapshot, release, err := v.Snapshot()
		if err != nil {
			continue // view is shut down; continue with others
		}
		defer release()
		snapshots = append(snapshots, snapshot)
	}
	s.viewMu.Unlock()

	// Expand the modification to any file we could care about, which we define
	// to be any file observed by any of the snapshots.
	//
	// There may be other files in the directory, but if we haven't read them yet
	// we don't need to invalidate them.
	var result []file.Modification
	for _, c := range changes {
		expanded := make(map[protocol.DocumentURI]bool)
		for _, snapshot := range snapshots {
			for _, uri := range snapshot.filesInDir(c.URI) {
				expanded[uri] = true
			}
		}
		if len(expanded) == 0 {
			result = append(result, c)
		} else {
			for uri := range expanded {
				result = append(result, file.Modification{
					URI:        uri,
					Action:     c.Action,
					LanguageID: "",
					OnDisk:     c.OnDisk,
					// changes to directories cannot include text or versions
				})
			}
		}
	}
	return result
}

// Precondition: caller holds s.viewMu lock.
// TODO(rfindley): move this to fs_overlay.go.
func (fs *overlayFS) updateOverlays(ctx context.Context, changes []file.Modification) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	for _, c := range changes {
		o, ok := fs.overlays[c.URI]

		// If the file is not opened in an overlay and the change is on disk,
		// there's no need to update an overlay. If there is an overlay, we
		// may need to update the overlay's saved value.
		if !ok && c.OnDisk {
			continue
		}

		// Determine the file kind on open, otherwise, assume it has been cached.
		var kind file.Kind
		switch c.Action {
		case file.Open:
			kind = file.KindForLang(c.LanguageID)
		default:
			if !ok {
				return fmt.Errorf("updateOverlays: modifying unopened overlay %v", c.URI)
			}
			kind = o.kind
		}

		// Closing a file just deletes its overlay.
		if c.Action == file.Close {
			delete(fs.overlays, c.URI)
			continue
		}

		// If the file is on disk, check if its content is the same as in the
		// overlay. Saves and on-disk file changes don't come with the file's
		// content.
		text := c.Text
		if text == nil && (c.Action == file.Save || c.OnDisk) {
			if !ok {
				return fmt.Errorf("no known content for overlay for %s", c.Action)
			}
			text = o.content
		}
		// On-disk changes don't come with versions.
		version := c.Version
		if c.OnDisk || c.Action == file.Save {
			version = o.version
		}
		hash := file.HashOf(text)
		var sameContentOnDisk bool
		switch c.Action {
		case file.Delete:
			// Do nothing. sameContentOnDisk should be false.
		case file.Save:
			// Make sure the version and content (if present) is the same.
			if false && o.version != version { // Client no longer sends the version
				return fmt.Errorf("updateOverlays: saving %s at version %v, currently at %v", c.URI, c.Version, o.version)
			}
			if c.Text != nil && o.hash != hash {
				return fmt.Errorf("updateOverlays: overlay %s changed on save", c.URI)
			}
			sameContentOnDisk = true
		default:
			fh := mustReadFile(ctx, fs.delegate, c.URI)
			_, readErr := fh.Content()
			sameContentOnDisk = (readErr == nil && fh.Identity().Hash == hash)
		}
		o = &Overlay{
			uri:     c.URI,
			version: version,
			content: text,
			kind:    kind,
			hash:    hash,
			saved:   sameContentOnDisk,
		}

		// NOTE: previous versions of this code checked here that the overlay had a
		// view and file kind (but we don't know why).

		fs.overlays[c.URI] = o
	}

	return nil
}

func mustReadFile(ctx context.Context, fs file.Source, uri protocol.DocumentURI) file.Handle {
	ctx = xcontext.Detach(ctx)
	fh, err := fs.ReadFile(ctx, uri)
	if err != nil {
		// ReadFile cannot fail with an uncancellable context.
		bug.Reportf("reading file failed unexpectedly: %v", err)
		return brokenFile{uri, err}
	}
	return fh
}

// A brokenFile represents an unexpected failure to read a file.
type brokenFile struct {
	uri protocol.DocumentURI
	err error
}

func (b brokenFile) URI() protocol.DocumentURI { return b.uri }
func (b brokenFile) Identity() file.Identity   { return file.Identity{URI: b.uri} }
func (b brokenFile) SameContentsOnDisk() bool  { return false }
func (b brokenFile) Version() int32            { return 0 }
func (b brokenFile) Content() ([]byte, error)  { return nil, b.err }

// FileWatchingGlobPatterns returns a set of glob patterns patterns that the
// client is required to watch for changes, and notify the server of them, in
// order to keep the server's state up to date.
//
// This set includes
//  1. all go.mod and go.work files in the workspace; and
//  2. for each Snapshot, its modules (or directory for ad-hoc views). In
//     module mode, this is the set of active modules (and for VS Code, all
//     workspace directories within them, due to golang/go#42348).
//
// The watch for workspace go.work and go.mod files in (1) is sufficient to
// capture changes to the repo structure that may affect the set of views.
// Whenever this set changes, we reload the workspace and invalidate memoized
// files.
//
// The watch for workspace directories in (2) should keep each View up to date,
// as it should capture any newly added/modified/deleted Go files.
//
// TODO(golang/go#57979): we need to reset the memoizedFS when a view changes.
// Consider the case where we incidentally read a file, then it moved outside
// of an active module, and subsequently changed: we would still observe the
// original file state.
func (s *Session) FileWatchingGlobPatterns(ctx context.Context) map[string]unit {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()

	// Always watch files that may change the set of views.
	patterns := map[string]unit{
		"**/*.{mod,work}": {},
	}

	for _, view := range s.views {
		snapshot, release, err := view.Snapshot()
		if err != nil {
			continue // view is shut down; continue with others
		}
		for k, v := range snapshot.fileWatchingGlobPatterns(ctx) {
			patterns[k] = v
		}
		release()
	}
	return patterns
}

// OrphanedFileDiagnostics reports diagnostics describing why open files have
// no packages or have only command-line-arguments packages.
//
// If the resulting diagnostic is nil, the file is either not orphaned or we
// can't produce a good diagnostic.
//
// The caller must not mutate the result.
func (s *Session) OrphanedFileDiagnostics(ctx context.Context) (map[protocol.DocumentURI][]*Diagnostic, error) {
	// Note: diagnostics holds a slice for consistency with other diagnostic
	// funcs.
	diagnostics := make(map[protocol.DocumentURI][]*Diagnostic)

	byView := make(map[*View][]*Overlay)
	for _, o := range s.Overlays() {
		uri := o.URI()
		snapshot, release, err := s.SnapshotOf(ctx, uri)
		if err != nil {
			// TODO(golang/go#57979): we have to use the .go suffix as an approximation for
			// file kind here, because we don't have access to Options if no View was
			// matched.
			//
			// But Options are really a property of Folder, not View, and we could
			// match a folder here.
			//
			// Refactor so that Folders are tracked independently of Views, and use
			// the correct options here to get the most accurate file kind.
			//
			// TODO(golang/go#57979): once we switch entirely to the zeroconfig
			// logic, we should use this diagnostic for the fallback case of
			// s.views[0] in the ViewOf logic.
			if errors.Is(err, errNoViews) {
				if strings.HasSuffix(string(uri), ".go") {
					if _, rng, ok := orphanedFileDiagnosticRange(ctx, s.parseCache, o); ok {
						diagnostics[uri] = []*Diagnostic{{
							URI:      uri,
							Range:    rng,
							Severity: protocol.SeverityWarning,
							Source:   ListError,
							Message:  fmt.Sprintf("No active builds contain %s: consider opening a new workspace folder containing it", uri.Path()),
						}}
					}
				}
				continue
			}
			return nil, err
		}
		v := snapshot.View()
		release()
		byView[v] = append(byView[v], o)
	}

	for view, overlays := range byView {
		snapshot, release, err := view.Snapshot()
		if err != nil {
			continue // view is shutting down
		}
		defer release()
		diags, err := snapshot.orphanedFileDiagnostics(ctx, overlays)
		if err != nil {
			return nil, err
		}
		for _, d := range diags {
			diagnostics[d.URI] = append(diagnostics[d.URI], d)
		}
	}
	return diagnostics, nil
}
