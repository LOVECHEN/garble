package main

import (
	"fmt"
	"go/ast"
	"go/types"
	mathrand "math/rand"
	"strings"
)

// cmd/bundle will include a go:generate directive in its output by default.
// Ours specifies a version and doesn't assume bundle is in $PATH, so drop it.

//go:generate go tool bundle -o cmdgo_quoted.go -prefix cmdgoQuoted cmd/internal/quoted
//go:generate sed -i /go:generate/d cmdgo_quoted.go

// computeLinkerVariableStrings iterates over the -ldflags arguments,
// filling a map with all the string values set via the linker's -X flag.
// TODO: can we put this in sharedCache, using objectString as a key?
func computeLinkerVariableStrings(pkg *types.Package) (map[*types.Var]string, error) {
	linkerVariableStrings := make(map[*types.Var]string)

	// TODO: this is a linker flag that affects how we obfuscate a package at
	// compile time. Note that, if the user changes ldflags, then Go may only
	// re-link the final binary, without re-compiling any packages at all.
	// It's possible that this could result in:
	//
	//    garble -literals build -ldflags=-X=pkg.name=before # name="before"
	//    garble -literals build -ldflags=-X=pkg.name=after  # name="before" as cached
	//
	// We haven't been able to reproduce this problem for now,
	// but it's worth noting it and keeping an eye out for it in the future.
	// If we do confirm this theoretical bug,
	// the solution will be to either find a different solution for -literals,
	// or to force including -ldflags into the build cache key.
	ldflags, err := cmdgoQuotedSplit(flagValue(sharedCache.ForwardBuildFlags, "-ldflags"))
	if err != nil {
		return nil, err
	}
	flagValueIter(ldflags, "-X", func(val string) {
		// val is in the form of "foo.com/bar.name=value".
		fullName, stringValue, found := strings.Cut(val, "=")
		if !found {
			return // invalid
		}

		// fullName is "foo.com/bar.name"
		i := strings.LastIndexByte(fullName, '.')
		path, name := fullName[:i], fullName[i+1:]

		// Note that package main always has import path "main" as part of a build.
		if path != pkg.Path() && (path != "main" || pkg.Name() != "main") {
			return // not the current package
		}

		obj, _ := pkg.Scope().Lookup(name).(*types.Var)
		if obj == nil {
			return // no such variable; skip
		}
		linkerVariableStrings[obj] = stringValue
	})
	return linkerVariableStrings, nil
}

func typecheck(pkgPath string, files []*ast.File, origImporter importerWithMap) (*types.Package, *types.Info, error) {
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	origTypesConfig := types.Config{
		// Note that we don't set GoVersion here. Any Go language version checks
		// are performed by the upfront `go list -json -compiled` call.
		Importer: origImporter,
		Sizes:    types.SizesFor("gc", sharedCache.GoEnv.GOARCH),
	}
	pkg, err := origTypesConfig.Check(pkgPath, fset, files, info)
	if err != nil {
		return nil, nil, fmt.Errorf("typecheck error: %v", err)
	}
	return pkg, info, err
}

func computeFieldToStruct(info *types.Info) map[*types.Var]*types.Struct {
	done := make(map[*types.Named]bool)
	fieldToStruct := make(map[*types.Var]*types.Struct)

	// Run recordType on all types reachable via types.Info.
	// A bit hacky, but I could not find an easier way to do this.
	for _, obj := range info.Uses {
		if obj != nil {
			recordType(obj.Type(), nil, done, fieldToStruct)
		}
	}
	for _, obj := range info.Defs {
		if obj != nil {
			recordType(obj.Type(), nil, done, fieldToStruct)
		}
	}
	for _, tv := range info.Types {
		recordType(tv.Type, nil, done, fieldToStruct)
	}
	return fieldToStruct
}

// recordType visits every reachable type after typechecking a package.
// Right now, all it does is fill the fieldToStruct map.
// Since types can be recursive, we need a map to avoid cycles.
// We only need to track named types as done, as all cycles must use them.
func recordType(used, origin types.Type, done map[*types.Named]bool, fieldToStruct map[*types.Var]*types.Struct) {
	used = types.Unalias(used)
	if origin == nil {
		origin = used
	} else {
		origin = types.Unalias(origin)
		// origin may be a [*types.TypeParam].
		// For now, we haven't found a need to recurse in that case.
		// We can edit this code in the future if we find an example,
		// because we panic if a field is not in fieldToStruct.
		if _, ok := origin.(*types.TypeParam); ok {
			return
		}
	}
	type Container interface{ Elem() types.Type }
	switch used := used.(type) {
	case Container:
		recordType(used.Elem(), origin.(Container).Elem(), done, fieldToStruct)
	case *types.Named:
		if done[used] {
			return
		}
		done[used] = true
		// If we have a generic struct like
		//
		//	type Foo[T any] struct { Bar T }
		//
		// then we want the hashing to use the original "Bar T",
		// because otherwise different instances like "Bar int" and "Bar bool"
		// will result in different hashes and the field names will break.
		// Ensure we record the original generic struct, if there is one.
		recordType(used.Underlying(), used.Origin().Underlying(), done, fieldToStruct)
	case *types.Struct:
		origin := origin.(*types.Struct)
		for i := range used.NumFields() {
			field := used.Field(i)
			fieldToStruct[field.Origin()] = origin

			if field.Embedded() {
				originField := origin.Field(i)
				recordType(field.Type(), originField.Type(), done, fieldToStruct)
			}
		}
	}
}

// isSafeForInstanceType returns true if the passed type is safe for var declaration.
// Unsafe types: generic types and non-method interfaces.
func isSafeForInstanceType(t types.Type) bool {
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		return t.Kind() != types.Invalid
	case *types.Named:
		if t.TypeParams().Len() > 0 {
			return false
		}
		return isSafeForInstanceType(t.Underlying())
	case *types.Signature:
		return t.TypeParams().Len() == 0
	case *types.Interface:
		return t.IsMethodSet()
	}
	return true
}

// namedType tries to obtain the *types.TypeName behind a type, if there is one.
// This is useful to obtain "testing.T" from "*testing.T", or to obtain the type
// declaration object from an embedded field.
// Note that, for a type alias, this gives the alias name.
func namedType(t types.Type) *types.TypeName {
	switch t := t.(type) {
	case *types.Alias:
		return t.Obj()
	case *types.Named:
		return t.Obj()
	case *types.Pointer:
		return namedType(t.Elem())
	default:
		return nil
	}
}

// isTestSignature returns true if the signature matches "func _(*testing.T)".
func isTestSignature(sign *types.Signature) bool {
	if sign.Recv() != nil {
		return false // test funcs don't have receivers
	}
	params := sign.Params()
	if params.Len() != 1 {
		return false // too many parameters for a test func
	}
	tname := namedType(params.At(0).Type())
	if tname == nil {
		return false // the only parameter isn't named, like "string"
	}
	return tname.Pkg().Path() == "testing" && tname.Name() == "T"
}

func splitFlagsFromArgs(all []string) (flags, args []string) {
	for i := 0; i < len(all); i++ {
		arg := all[i]
		if !strings.HasPrefix(arg, "-") {
			return all[:i:i], all[i:]
		}
		if booleanFlags[arg] || strings.Contains(arg, "=") {
			// Either "-bool" or "-name=value".
			continue
		}
		// "-name value", so the next arg is part of this flag.
		i++
	}
	return all, nil
}

func alterTrimpath(flags []string) []string {
	trimpath := flagValue(flags, "-trimpath")

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	return flagSetValue(flags, "-trimpath", sharedTempDir+"=>;"+trimpath)
}

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// curPkg holds basic information about the package being currently compiled or linked.
	curPkg *listedPackage

	// curPkgCache is the pkgCache for curPkg.
	curPkgCache pkgCache

	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

	// linkerVariableStrings records objects for variables used in -ldflags=-X flags,
	// as well as the strings the user wants to inject them with.
	// Used when obfuscating literals, so that we obfuscate the injected value.
	linkerVariableStrings map[*types.Var]string

	// fieldToStruct helps locate struct types from any of their field
	// objects. Useful when obfuscating field names.
	fieldToStruct map[*types.Var]*types.Struct

	// obfRand is initialized by transformCompile and used during obfuscation.
	// It is left nil at init time, so that we only use it after it has been
	// properly initialized with a deterministic seed.
	// It must only be used for deterministic obfuscation;
	// if it is used for any other purpose, we may lose determinism.
	obfRand *mathrand.Rand

	// origImporter is a go/types importer which uses the original versions
	// of packages, without any obfuscation. This is helpful to make
	// decisions on how to obfuscate our input code.
	origImporter importerWithMap

	// usedAllImportsFiles is used to prevent multiple calls of tf.useAllImports function on one file
	// in case of simultaneously applied control flow and literals obfuscation
	usedAllImportsFiles map[*ast.File]bool
}

var transformMethods = map[string]func(*transformer, []string) ([]string, error){
	"asm":     (*transformer).transformAsm,
	"compile": (*transformer).transformCompile,
	"link":    (*transformer).transformLink,
}
