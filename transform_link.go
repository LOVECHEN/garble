package main

import (
	"fmt"
	"strings"
)

func (tf *transformer) transformLink(args []string) ([]string, error) {
	// We can't split by the ".a" extension, because cached object files
	// lack any extension.
	flags, args := splitFlagsFromArgs(args)

	newImportCfg, err := tf.processImportCfg(flags, nil)
	if err != nil {
		return nil, err
	}

	// TODO: unify this logic with the -X handling when using -literals.
	// We should be able to handle both cases via the syntax tree.
	//
	// Make sure -X works with obfuscated identifiers.
	// To cover both obfuscated and non-obfuscated names,
	// duplicate each flag with a obfuscated version.
	flagValueIter(flags, "-X", func(val string) {
		// val is in the form of "foo.com/bar.name=value".
		fullName, stringValue, found := strings.Cut(val, "=")
		if !found {
			return // invalid
		}

		// fullName is "foo.com/bar.name"
		i := strings.LastIndexByte(fullName, '.')
		path, name := fullName[:i], fullName[i+1:]

		// If the package path is "main", it's the current top-level
		// package we are linking. Otherwise, find it in the cache.
		lpkg := tf.curPkg
		if path != "main" {
			lpkg = sharedCache.ListedPackages[path]
		}
		if lpkg == nil {
			// We couldn't find the package.
			// Perhaps a typo, perhaps not part of the build.
			// cmd/link ignores those, so we should too.
			return
		}
		newName := hashWithPackage(lpkg, name)
		flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", lpkg.obfuscatedImportPath(), newName, stringValue))
	})

	// Starting in Go 1.17, Go's version is implicitly injected by the linker.
	// It's the same method as -X, so we can override it with an extra flag.
	flags = append(flags, "-X=runtime.buildVersion=unknown")

	// Ensure we strip the -buildid flag, to not leak any build IDs for the
	// link operation or the main package's compilation.
	flags = flagSetValue(flags, "-buildid", "")

	// Strip debug information and symbol tables.
	flags = append(flags, "-w", "-s")

	flags = flagSetValue(flags, "-importcfg", newImportCfg)
	return append(flags, args...), nil
}
