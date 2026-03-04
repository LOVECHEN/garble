package main

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"regexp"
	"strings"
)

// forwardBuildFlags is obtained from 'go help build' as of Go 1.21.
var forwardBuildFlags = map[string]bool{
	// These shouldn't be used in nested cmd/go calls.
	"-a": false,
	"-n": false,
	"-x": false,
	"-v": false,

	// These are always set by garble.
	"-trimpath": false,
	"-toolexec": false,
	"-buildvcs": false,

	"-C":             true,
	"-asan":          true,
	"-asmflags":      true,
	"-buildmode":     true,
	"-compiler":      true,
	"-cover":         true,
	"-covermode":     true,
	"-coverpkg":      true,
	"-gccgoflags":    true,
	"-gcflags":       true,
	"-installsuffix": true,
	"-ldflags":       true,
	"-linkshared":    true,
	"-mod":           true,
	"-modcacherw":    true,
	"-modfile":       true,
	"-msan":          true,
	"-overlay":       true,
	"-p":             true,
	"-pgo":           true,
	"-pkgdir":        true,
	"-race":          true,
	"-tags":          true,
	"-work":          true,
	"-workfile":      true,
}

// booleanFlags is obtained from 'go help build' and 'go help testflag' as of Go 1.21.
var booleanFlags = map[string]bool{
	// Shared build flags.
	"-a":          true,
	"-asan":       true,
	"-buildvcs":   true,
	"-cover":      true,
	"-i":          true,
	"-linkshared": true,
	"-modcacherw": true,
	"-msan":       true,
	"-n":          true,
	"-race":       true,
	"-trimpath":   true,
	"-v":          true,
	"-work":       true,
	"-x":          true,

	// Test flags (TODO: support its special -args flag)
	"-benchmem": true,
	"-c":        true,
	"-failfast": true,
	"-fullpath": true,
	"-json":     true,
	"-short":    true,
}

var flagSet = flag.NewFlagSet("garble", flag.ExitOnError)
var rxGarbleFlag = regexp.MustCompile(`-(?:literals|tiny|debug|debugdir|seed)(?:$|=)`)

type seedFlag struct {
	random bool
	bytes  []byte
}

func (f seedFlag) present() bool { return len(f.bytes) > 0 }

func (f seedFlag) String() string {
	return base64.RawStdEncoding.EncodeToString(f.bytes)
}

func (f *seedFlag) Set(s string) error {
	if s == "random" {
		f.random = true // to show the random seed we chose

		f.bytes = make([]byte, 16) // random 128 bit seed
		if _, err := cryptorand.Read(f.bytes); err != nil {
			return fmt.Errorf("error generating random seed: %v", err)
		}
	} else {
		// We expect unpadded base64, but to be nice, accept padded
		// strings too.
		s = strings.TrimRight(s, "=")
		seed, err := base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("error decoding seed: %v", err)
		}

		// TODO: Note that we always use 8 bytes; any bytes after that are
		// entirely ignored. That may be confusing to the end user.
		if len(seed) < 8 {
			return fmt.Errorf("-seed needs at least 8 bytes, have %d", len(seed))
		}
		f.bytes = seed
	}
	return nil
}

func filterForwardBuildFlags(flags []string) (filtered []string, firstUnknown string) {
	for i := 0; i < len(flags); i++ {
		arg := flags[i]
		if strings.HasPrefix(arg, "--") {
			arg = arg[1:] // "--name" to "-name"; keep the short form
		}

		name, _, _ := strings.Cut(arg, "=") // "-name=value" to "-name"

		buildFlag := forwardBuildFlags[name]
		if buildFlag {
			filtered = append(filtered, arg)
		} else {
			firstUnknown = name
		}
		if booleanFlags[arg] || strings.Contains(arg, "=") {
			// Either "-bool" or "-name=value".
			continue
		}
		// "-name value", so the next arg is part of this flag.
		if i++; buildFlag && i < len(flags) {
			filtered = append(filtered, flags[i])
		}
	}
	return filtered, firstUnknown
}

// splitFlagsFromFiles splits args into a list of flag and file arguments. Since
// we can't rely on "--" being present, and we don't parse all flags upfront, we
// rely on finding the first argument that doesn't begin with "-" and that has
// the extension we expect for the list of paths.
//
// This function only makes sense for lower-level tool commands, such as
// "compile" or "link", since their arguments are predictable.
//
// We iterate from the end rather than from the start, to better protect
// oursrelves from flag arguments that may look like paths, such as:
//
//	compile [flags...] -p pkg/path.go [more flags...] file1.go file2.go
//
// For now, since those confusing flags are always followed by more flags,
// iterating in reverse order works around them entirely.
func splitFlagsFromFiles(all []string, ext string) (flags, paths []string) {
	for i := len(all) - 1; i >= 0; i-- {
		arg := all[i]
		if strings.HasPrefix(arg, "-") || !strings.HasSuffix(arg, ext) {
			cutoff := i + 1 // arg is a flag, not a path
			return all[:cutoff:cutoff], all[cutoff:]
		}
	}
	return nil, all
}

// flagValue retrieves the value of a flag such as "-foo", from strings in the
// list of arguments like "-foo=bar" or "-foo" "bar". If the flag is repeated,
// the last value is returned.
func flagValue(flags []string, name string) string {
	lastVal := ""
	flagValueIter(flags, name, func(val string) {
		lastVal = val
	})
	return lastVal
}

// flagValueIter retrieves all the values for a flag such as "-foo", like
// flagValue. The difference is that it allows handling complex flags, such as
// those whose values compose a list.
func flagValueIter(flags []string, name string, fn func(string)) {
	for i, arg := range flags {
		if val, ok := strings.CutPrefix(arg, name+"="); ok {
			// -name=value
			fn(val)
		}
		if arg == name { // -name ...
			if i+1 < len(flags) {
				// -name value
				fn(flags[i+1])
			}
		}
	}
}

func flagSetValue(flags []string, name, value string) []string {
	for i, arg := range flags {
		if strings.HasPrefix(arg, name+"=") {
			// -name=value
			flags[i] = name + "=" + value
			return flags
		}
		if arg == name { // -name ...
			if i+1 < len(flags) {
				// -name value
				flags[i+1] = value
				return flags
			}
			return flags
		}
	}
	return append(flags, name+"="+value)
}

func hasHelpFlag(flags []string) bool {
	for _, f := range flags {
		switch f {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}
