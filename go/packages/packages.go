// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package packages

// See doc.go for package documentation and implementation notes.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"sync"

	"golang.org/x/tools/go/gcexportdata"
)

// A LoadMode specifies the amount of detail to return when loading packages.
// The modes are all strictly additive, as you go through the list it increases
// the amount of information available to you, but may also increase the cost
// of collecting the information.
// Load is always allowed to return more information than requested.
type LoadMode int

const (
	// LoadFiles finds the packages and computes their source file lists.
	// Package fields: ID, Name, Errors, GoFiles, OtherFiles.
	LoadFiles LoadMode = iota

	// LoadImports adds import information for each package
	// and its dependencies.
	// Package fields added: Imports.
	LoadImports

	// LoadTypes adds type information for the package's exported symbols.
	// Package fields added: Types, Fset, IllTyped.
	// This will use the type information provided by the build system if
	// possible, and the ExportFile field may be filled in.
	LoadTypes

	// LoadSyntax adds typed syntax trees for the packages matching the patterns.
	// Package fields added: Syntax, TypesInfo, for direct pattern matches only.
	LoadSyntax

	// LoadAllSyntax adds typed syntax trees for the packages matching the patterns
	// and all dependencies.
	// Package fields added: Syntax, TypesInfo, for all packages in import graph.
	LoadAllSyntax
)

// An Config specifies details about how packages should be loaded.
// Calls to Load do not modify this struct.
type Config struct {
	// Mode controls the level of information returned for each package.
	Mode LoadMode

	// Context specifies the context for the load operation.
	// If the context is cancelled, the loader may stop early
	// and return an ErrCancelled error.
	// If Context is nil, the load cannot be cancelled.
	Context context.Context

	// Dir is the directory in which to run the build system tool
	// that provides information about the packages.
	// If Dir is empty, the tool is run in the current directory.
	Dir string

	// Env is the environment to use when invoking the build system tool.
	// If Env is nil, the current environment is used.
	// Like in os/exec's Cmd, only the last value in the slice for
	// each environment key is used. To specify the setting of only
	// a few variables, append to the current environment, as in:
	//
	//	opt.Env = append(os.Environ(), "GOOS=plan9", "GOARCH=386")
	//
	Env []string

	// Flags is a list of command-line flags to be passed through to
	// the underlying query tool.
	Flags []string

	// Error is called for each error encountered during package loading.
	// It must be safe to call Error simultaneously from multiple goroutines.
	// In addition to calling Error, the loader will record each error
	// in the corresponding Package's Errors list.
	// If Error is nil, the loader will print errors to os.Stderr.
	// To disable printing of errors, set opt.Error = func(error){}.
	// TODO(rsc): What happens in the Metadata loader? Currently nothing.
	Error func(error)

	// Fset is the token.FileSet to use when parsing source files or
	// type information provided by the build system.
	// If Fset is nil, the loader will create one.
	Fset *token.FileSet

	// ParseFile is called to read and parse each file
	// when preparing a package's type-checked syntax tree.
	// It must be safe to call ParseFile simultaneously from multiple goroutines.
	// If ParseFile is nil, the loader will uses parser.ParseFile.
	//
	// Setting ParseFile to a custom implementation can allow
	// providing alternate file content in order to type-check
	// unsaved text editor buffers, or to selectively eliminate
	// unwanted function bodies to reduce the amount of work
	// done by the type checker.
	ParseFile func(fset *token.FileSet, filename string) (*ast.File, error)

	// If Tests is set, the loader includes not just the packages
	// matching a particular pattern but also any related test packages,
	// including test-only variants of the package and the test executable.
	//
	// For example, when using the go command, loading "fmt" with Tests=true
	// returns four packages, with IDs "fmt" (the standard package),
	// "fmt [fmt.test]" (the package as compiled for the test),
	// "fmt_test" (the test functions from source files in package fmt_test),
	// and "fmt.test" (the test binary).
	//
	// In build systems with explicit names for tests,
	// setting Tests may have no effect.
	Tests bool

	// TypeChecker provides additional configuration for type-checking syntax trees.
	//
	// It is used for all packages in LoadAllSyntax mode,
	// and for the packages matching the patterns, but not their dependencies,
	// in LoadSyntax mode.
	//
	// The TypeChecker.Error function is ignored:
	// errors are reported using the Error function defined above.
	//
	// The TypeChecker.Importer function is ignored:
	// the loader defines an appropriate importer.
	//
	// TODO(rsc): TypeChecker.Sizes should use the same sizes as the main build.
	// Derive them from the runtime?
	TypeChecker types.Config
}

// driver is the type for functions that return the package structure as
// provided by a build system for the packages named by the given patterns.
type driver func(cfg *Config, patterns ...string) (*driverResponse, error)

// driverResponse contains the results for a driver query.
type driverResponse struct {
	// Roots is the set of package IDs that make up the root packages.
	// We have to encode this separately because when we encode a single package
	// we cannot know if it is one of the roots, that requires knowledge of the
	// graph it is part of.
	Roots []string `json:",omitempty"`

	// Packages is the full set of packages in the graph.
	// The packages are not connected into a graph, the Imports if populated will be
	// stubs that only have their ID set.
	// It will be connected and then type and syntax information added in a later
	// pass (see refine).
	Packages []*Package
}

// Load and returns the Go packages named by the given patterns.
//
// Config specifies loading options;
// nil behaves the same as an empty Config.
//
// Load returns an error if any of the patterns was invalid
// as defined by the underlying build system.
// It may return an empty list of packages without an error,
// for instance for an empty expansion of a valid wildcard.
func Load(cfg *Config, patterns ...string) ([]*Package, error) {
	l := newLoader(cfg)
	response, err := defaultDriver(&l.Config, patterns...)
	if err != nil {
		return nil, err
	}
	return l.refine(response.Roots, response.Packages...)
}

// defaultDriver is a driver that looks for an external driver binary, and if
// it does not find it falls back to the built in go list driver.
func defaultDriver(cfg *Config, patterns ...string) (*driverResponse, error) {
	driver := findExternalDriver(cfg)
	if driver == nil {
		driver = goListDriver
	}
	return driver(cfg, patterns...)
}

// A Package describes a single loaded Go package.
type Package struct {
	// ID is a unique identifier for a package,
	// in a syntax provided by the underlying build system.
	//
	// Because the syntax varies based on the build system,
	// clients should treat IDs as opaque and not attempt to
	// interpret them.
	ID string

	// Name is the package name as it appears in the package source code.
	Name string

	// This is the package path as used by the types package.
	// This is used to map entries in the type information back to the package
	// they come from.
	PkgPath string

	// Errors lists any errors encountered while loading the package.
	// TODO(rsc): Say something about the errors or at least their Strings,
	// as far as file:line being at the beginning and so on.
	Errors []error

	// GoFiles lists the absolute file paths of the package's Go source files.
	GoFiles []string

	// CompiledGoFiles lists the absolute file paths of the package's source
	// files that were presented to the compiler.
	// This may differ from GoFiles if files are processed before compilation.
	CompiledGoFiles []string

	// OtherFiles lists the absolute file paths of the package's non-Go source files,
	// including assembly, C, C++, Fortran, Objective-C, SWIG, and so on.
	OtherFiles []string

	// ExportFile is the absolute path to a file containing the type information
	// provided by the build system.
	ExportFile string

	// Imports maps import paths appearing in the package's Go source files
	// to corresponding loaded Packages.
	Imports map[string]*Package

	// Types is the type information for the package.
	// Modes LoadTypes and above set this field for all packages.
	//
	// TODO(adonovan): all packages? In Types mode this entails
	// asymptotically more export data processing than is required
	// to load the requested packages. Is that what we want?
	Types *types.Package

	// Fset provides position information for Types, TypesInfo, and Syntax.
	// Modes LoadTypes and above set this field for all packages.
	Fset *token.FileSet

	// IllTyped indicates whether the package has any type errors.
	// Modes LoadTypes and above set this field for all packages.
	IllTyped bool

	// Syntax is the package's syntax trees, for the files listed in GoFiles.
	//
	// Mode LoadSyntax set this field for packages matching the patterns.
	// Mode LoadSyntaxAll sets this field for all packages, including dependencies.
	Syntax []*ast.File

	// TypesInfo is the type-checking results for the package's syntax trees.
	// It is set only when Syntax is set.
	TypesInfo *types.Info
}

// packageError is used to serialize structured errors as much as possible.
// This has members compatible with the golist error type, and possibly some
// more if we need other error information to survive.
type packageError struct {
	Pos string // position of error
	Err string // the error itself
}

func (e *packageError) Error() string {
	return e.Pos + ": " + e.Err
}

// flatPackage is the JSON form of Package
// It drops all the type and syntax fields, and transforms the Imports and Errors
type flatPackage struct {
	ID              string
	Name            string            `json:",omitempty"`
	PkgPath         string            `json:",omitempty"`
	Errors          []*packageError   `json:",omitempty"`
	GoFiles         []string          `json:",omitempty"`
	CompiledGoFiles []string          `json:",omitempty"`
	OtherFiles      []string          `json:",omitempty"`
	ExportFile      string            `json:",omitempty"`
	Imports         map[string]string `json:",omitempty"`
}

// MarshalJSON returns the Package in its JSON form.
// For the most part, the structure fields are written out unmodified, and
// the type and syntax fields are skipped.
// The imports are written out as just a map of path to package id.
// The errors are written using a custom type that tries to preserve the
// structure of error types we know about.
// This method exists to enable support for additional build systems.  It is
// not intended for use by clients of the API and we may change the format.
func (p *Package) MarshalJSON() ([]byte, error) {
	flat := &flatPackage{
		ID:              p.ID,
		Name:            p.Name,
		PkgPath:         p.PkgPath,
		GoFiles:         p.GoFiles,
		CompiledGoFiles: p.CompiledGoFiles,
		OtherFiles:      p.OtherFiles,
		ExportFile:      p.ExportFile,
	}
	if len(p.Errors) > 0 {
		flat.Errors = make([]*packageError, len(p.Errors))
		for i, err := range p.Errors {
			//TODO: best effort mapping of errors to the serialized form
			switch err := err.(type) {
			case *packageError:
				flat.Errors[i] = err
			default:
				flat.Errors[i] = &packageError{Err: err.Error()}
			}
		}
	}
	if len(p.Imports) > 0 {
		flat.Imports = make(map[string]string, len(p.Imports))
		for path, ipkg := range p.Imports {
			flat.Imports[path] = ipkg.ID
		}
	}
	return json.Marshal(flat)
}

// UnmarshalJSON reads in a Package from its JSON format.
// See MarshalJSON for details about the format accepted.
func (p *Package) UnmarshalJSON(b []byte) error {
	flat := &flatPackage{}
	if err := json.Unmarshal(b, &flat); err != nil {
		return err
	}
	*p = Package{
		ID:              flat.ID,
		Name:            flat.Name,
		PkgPath:         flat.PkgPath,
		GoFiles:         flat.GoFiles,
		CompiledGoFiles: flat.CompiledGoFiles,
		OtherFiles:      flat.OtherFiles,
		ExportFile:      flat.ExportFile,
	}
	if len(flat.Errors) >= 0 {
		p.Errors = make([]error, len(flat.Errors))
		for i, err := range flat.Errors {
			p.Errors[i] = err
		}
	}
	if len(flat.Imports) > 0 {
		p.Imports = make(map[string]*Package, len(flat.Imports))
		for path, id := range flat.Imports {
			p.Imports[path] = &Package{ID: id}
		}
	}
	return nil
}

func (p *Package) String() string { return p.ID }

// loaderPackage augments Package with state used during the loading phase
type loaderPackage struct {
	*Package
	importErrors  map[string]error // maps each bad import to its error
	loadOnce      sync.Once
	color         uint8 // for cycle detection
	mark, needsrc bool  // used when Mode >= LoadTypes
}

// loader holds the working state of a single call to load.
type loader struct {
	pkgs map[string]*loaderPackage
	Config
	exportMu sync.Mutex // enforces mutual exclusion of exportdata operations
}

func newLoader(cfg *Config) *loader {
	ld := &loader{}
	if cfg != nil {
		ld.Config = *cfg
	}
	if ld.Context == nil {
		ld.Context = context.Background()
	}
	if ld.Dir == "" {
		if dir, err := os.Getwd(); err == nil {
			ld.Dir = dir
		}
	}

	if ld.Mode >= LoadTypes {
		if ld.Fset == nil {
			ld.Fset = token.NewFileSet()
		}

		// Error and ParseFile are required even in LoadTypes mode
		// because we load source if export data is missing.

		if ld.Error == nil {
			ld.Error = func(e error) {
				fmt.Fprintln(os.Stderr, e)
			}
		}

		if ld.ParseFile == nil {
			ld.ParseFile = func(fset *token.FileSet, filename string) (*ast.File, error) {
				const mode = parser.AllErrors | parser.ParseComments
				return parser.ParseFile(fset, filename, nil, mode)
			}
		}
	}
	return ld
}

// refine connects the supplied packages into a graph and then adds type and
// and syntax information as requested by the LoadMode.
func (ld *loader) refine(roots []string, list ...*Package) ([]*Package, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("packages not found")
	}
	isRoot := make(map[string]bool, len(roots))
	for _, root := range roots {
		isRoot[root] = true
	}
	ld.pkgs = make(map[string]*loaderPackage)
	// first pass, fixup and build the map and roots
	var initial []*loaderPackage
	for _, pkg := range list {
		lpkg := &loaderPackage{
			Package: pkg,
			needsrc: ld.Mode >= LoadAllSyntax ||
				(ld.Mode >= LoadSyntax && isRoot[pkg.ID]) ||
				(pkg.ExportFile == "" && pkg.PkgPath != "unsafe"),
		}
		ld.pkgs[lpkg.ID] = lpkg
		if isRoot[lpkg.ID] {
			initial = append(initial, lpkg)
		}
	}

	// Materialize the import graph.

	const (
		white = 0 // new
		grey  = 1 // in progress
		black = 2 // complete
	)

	// visit traverses the import graph, depth-first,
	// and materializes the graph as Packages.Imports.
	//
	// Valid imports are saved in the Packages.Import map.
	// Invalid imports (cycles and missing nodes) are saved in the importErrors map.
	// Thus, even in the presence of both kinds of errors, the Import graph remains a DAG.
	//
	// visit returns whether the package needs src or has a transitive
	// dependency on a package that does. These are the only packages
	// for which we load source code.
	var stack []*loaderPackage
	var visit func(lpkg *loaderPackage) bool
	visit = func(lpkg *loaderPackage) bool {
		switch lpkg.color {
		case black:
			return lpkg.needsrc
		case grey:
			panic("internal error: grey node")
		}
		lpkg.color = grey
		stack = append(stack, lpkg) // push
		stubs := lpkg.Imports       // the structure form has only stubs with the ID in the Imports
		lpkg.Imports = make(map[string]*Package, len(stubs))
		for importPath, ipkg := range stubs {
			var importErr error
			imp := ld.pkgs[ipkg.ID]
			if imp == nil {
				// (includes package "C" when DisableCgo)
				importErr = fmt.Errorf("missing package: %q", ipkg.ID)
			} else if imp.color == grey {
				importErr = fmt.Errorf("import cycle: %s", stack)
			}
			if importErr != nil {
				if lpkg.importErrors == nil {
					lpkg.importErrors = make(map[string]error)
				}
				lpkg.importErrors[importPath] = importErr
				continue
			}

			if visit(imp) {
				lpkg.needsrc = true
			}
			lpkg.Imports[importPath] = imp.Package
		}

		stack = stack[:len(stack)-1] // pop
		lpkg.color = black

		return lpkg.needsrc
	}

	if ld.Mode < LoadImports {
		//TODO: we are throwing away correct information, is that the right thing to do?
		//we do this to drop the stub import packages that we are not even going to try to resolve
		for _, lpkg := range initial {
			lpkg.Imports = nil
		}
	} else {
		// For each initial package, create its import DAG.
		for _, lpkg := range initial {
			visit(lpkg)
		}
	}
	// Load type data if needed, starting at
	// the initial packages (roots of the import DAG).
	if ld.Mode >= LoadTypes {
		var wg sync.WaitGroup
		for _, lpkg := range initial {
			wg.Add(1)
			go func(lpkg *loaderPackage) {
				ld.loadRecursive(lpkg)
				wg.Done()
			}(lpkg)
		}
		wg.Wait()
	}

	result := make([]*Package, len(initial))
	for i, lpkg := range initial {
		result[i] = lpkg.Package
	}
	return result, nil
}

// loadRecursive loads the specified package and its dependencies,
// recursively, in parallel, in topological order.
// It is atomic and idempotent.
// Precondition: ld.Mode >= LoadTypes.
func (ld *loader) loadRecursive(lpkg *loaderPackage) {
	lpkg.loadOnce.Do(func() {
		// Load the direct dependencies, in parallel.
		var wg sync.WaitGroup
		for _, ipkg := range lpkg.Imports {
			imp := ld.pkgs[ipkg.ID]
			wg.Add(1)
			go func(imp *loaderPackage) {
				ld.loadRecursive(imp)
				wg.Done()
			}(imp)
		}
		wg.Wait()

		ld.loadPackage(lpkg)
	})
}

// loadPackage loads the specified package.
// It must be called only once per Package,
// after immediate dependencies are loaded.
// Precondition: ld.Mode >= LoadTypes.
func (ld *loader) loadPackage(lpkg *loaderPackage) {
	if lpkg.PkgPath == "unsafe" {
		// Fill in the blanks to avoid surprises.
		lpkg.Types = types.Unsafe
		lpkg.Fset = ld.Fset
		lpkg.Syntax = []*ast.File{}
		lpkg.TypesInfo = new(types.Info)
		return
	}

	// Call NewPackage directly with explicit name.
	// This avoids skew between golist and go/types when the files'
	// package declarations are inconsistent.
	lpkg.Types = types.NewPackage(lpkg.PkgPath, lpkg.Name)

	if !lpkg.needsrc {
		ld.loadFromExportData(lpkg)
		return // not a source package, don't get syntax trees
	}

	hardErrors := false
	appendError := func(err error) {
		if terr, ok := err.(types.Error); ok && terr.Soft {
			// Don't mark the package as bad.
		} else {
			hardErrors = true
		}
		ld.Error(err)
		lpkg.Errors = append(lpkg.Errors, err)
	}

	files, errs := ld.parseFiles(lpkg.CompiledGoFiles)
	for _, err := range errs {
		appendError(err)
	}

	lpkg.Fset = ld.Fset
	lpkg.Syntax = files

	lpkg.TypesInfo = &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}

	// Copy the prototype types.Config as it must vary across Packages.
	tc := ld.TypeChecker // copy
	tc.Importer = importerFunc(func(path string) (*types.Package, error) {
		if path == "unsafe" {
			return types.Unsafe, nil
		}

		// The imports map is keyed by import path.
		ipkg := lpkg.Imports[path]
		if ipkg == nil {
			if err := lpkg.importErrors[path]; err != nil {
				return nil, err
			}
			// There was skew between the metadata and the
			// import declarations, likely due to an edit
			// race, or because the ParseFile feature was
			// used to supply alternative file contents.
			return nil, fmt.Errorf("no metadata for %s", path)
		}

		if ipkg.Types != nil && ipkg.Types.Complete() {
			return ipkg.Types, nil
		}
		log.Fatalf("internal error: nil Pkg importing %q from %q", path, lpkg)
		panic("unreachable")
	})
	tc.Error = appendError

	// type-check
	types.NewChecker(&tc, ld.Fset, lpkg.Types, lpkg.TypesInfo).Files(lpkg.Syntax)

	lpkg.importErrors = nil // no longer needed

	// If !Cgo, the type-checker uses FakeImportC mode, so
	// it doesn't invoke the importer for import "C",
	// nor report an error for the import,
	// or for any undefined C.f reference.
	// We must detect this explicitly and correctly
	// mark the package as IllTyped (by reporting an error).
	// TODO(adonovan): if these errors are annoying,
	// we could just set IllTyped quietly.
	if tc.FakeImportC {
	outer:
		for _, f := range lpkg.Syntax {
			for _, imp := range f.Imports {
				if imp.Path.Value == `"C"` {
					appendError(fmt.Errorf(`%s: import "C" ignored`,
						lpkg.Fset.Position(imp.Pos())))
					break outer
				}
			}
		}
	}

	// Record accumulated errors.
	for _, imp := range lpkg.Imports {
		if imp.IllTyped {
			hardErrors = true
			break
		}
	}

	lpkg.IllTyped = hardErrors
}

// An importFunc is an implementation of the single-method
// types.Importer interface based on a function value.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// We use a counting semaphore to limit
// the number of parallel I/O calls per process.
var ioLimit = make(chan bool, 20)

// parseFiles reads and parses the Go source files and returns the ASTs
// of the ones that could be at least partially parsed, along with a
// list of I/O and parse errors encountered.
//
// Because files are scanned in parallel, the token.Pos
// positions of the resulting ast.Files are not ordered.
//
func (ld *loader) parseFiles(filenames []string) ([]*ast.File, []error) {
	var wg sync.WaitGroup
	n := len(filenames)
	parsed := make([]*ast.File, n)
	errors := make([]error, n)
	for i, file := range filenames {
		wg.Add(1)
		go func(i int, filename string) {
			ioLimit <- true // wait
			// ParseFile may return both an AST and an error.
			parsed[i], errors[i] = ld.ParseFile(ld.Fset, filename)
			<-ioLimit // signal
			wg.Done()
		}(i, file)
	}
	wg.Wait()

	// Eliminate nils, preserving order.
	var o int
	for _, f := range parsed {
		if f != nil {
			parsed[o] = f
			o++
		}
	}
	parsed = parsed[:o]

	o = 0
	for _, err := range errors {
		if err != nil {
			errors[o] = err
			o++
		}
	}
	errors = errors[:o]

	return parsed, errors
}

// loadFromExportData returns type information for the specified
// package, loading it from an export data file on the first request.
func (ld *loader) loadFromExportData(lpkg *loaderPackage) (*types.Package, error) {
	if lpkg.PkgPath == "" {
		log.Fatalf("internal error: Package %s has no PkgPath", lpkg)
	}

	// Because gcexportdata.Read has the potential to create or
	// modify the types.Package for each node in the transitive
	// closure of dependencies of lpkg, all exportdata operations
	// must be sequential. (Finer-grained locking would require
	// changes to the gcexportdata API.)
	//
	// The exportMu lock guards the Package.Pkg field and the
	// types.Package it points to, for each Package in the graph.
	//
	// Not all accesses to Package.Pkg need to be protected by exportMu:
	// graph ordering ensures that direct dependencies of source
	// packages are fully loaded before the importer reads their Pkg field.
	ld.exportMu.Lock()
	defer ld.exportMu.Unlock()

	if tpkg := lpkg.Types; tpkg != nil && tpkg.Complete() {
		return tpkg, nil // cache hit
	}

	lpkg.IllTyped = true // fail safe

	if lpkg.ExportFile == "" {
		// Errors while building export data will have been printed to stderr.
		return nil, fmt.Errorf("no export data file")
	}
	f, err := os.Open(lpkg.ExportFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read gc export data.
	//
	// We don't currently support gccgo export data because all
	// underlying workspaces use the gc toolchain. (Even build
	// systems that support gccgo don't use it for workspace
	// queries.)
	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", lpkg.ExportFile, err)
	}

	// Build the view.
	//
	// The gcexportdata machinery has no concept of package ID.
	// It identifies packages by their PkgPath, which although not
	// globally unique is unique within the scope of one invocation
	// of the linker, type-checker, or gcexportdata.
	//
	// So, we must build a PkgPath-keyed view of the global
	// (conceptually ID-keyed) cache of packages and pass it to
	// gcexportdata. The view must contain every existing
	// package that might possibly be mentioned by the
	// current package---its transitive closure.
	//
	// TODO(adonovan): it would be more simpler and more efficient
	// if the export data machinery invoked a callback to
	// get-or-create a package instead of a map.
	//
	view := make(map[string]*types.Package) // view seen by gcexportdata
	seen := make(map[*loaderPackage]bool)   // all visited packages
	var visit func(pkgs map[string]*Package)
	visit = func(pkgs map[string]*Package) {
		for _, p := range pkgs {
			lpkg := ld.pkgs[p.ID]
			if !seen[lpkg] {
				seen[lpkg] = true
				view[lpkg.PkgPath] = lpkg.Types
				visit(lpkg.Imports)
			}
		}
	}
	visit(lpkg.Imports)

	// Parse the export data.
	// (May create/modify packages in view.)
	tpkg, err := gcexportdata.Read(r, ld.Fset, view, lpkg.PkgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", lpkg.ExportFile, err)
	}

	lpkg.Types = tpkg
	lpkg.IllTyped = false

	return tpkg, nil
}

func usesExportData(cfg *Config) bool {
	return LoadTypes <= cfg.Mode && cfg.Mode < LoadAllSyntax
}
