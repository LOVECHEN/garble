package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (tf *transformer) transformAsm(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output object file.
	flags = flagSetValue(flags, "-p", tf.curPkg.obfuscatedImportPath())

	flags = alterTrimpath(flags)

	// The assembler runs twice; the first with -gensymabis,
	// where we continue below and we obfuscate all the source.
	// The second time, without -gensymabis, we reconstruct the paths to the
	// obfuscated source files and reuse them to avoid work.
	newPaths := make([]string, 0, len(paths))
	if !slices.Contains(args, "-gensymabis") {
		for _, path := range paths {
			name := hashWithPackage(tf.curPkg, filepath.Base(path)) + ".s"
			pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedSourceDir())
			newPath := filepath.Join(pkgDir, name)
			newPaths = append(newPaths, newPath)
		}
		return append(flags, newPaths...), nil
	}

	const missingHeader = "missing header path"
	newHeaderPaths := make(map[string]string)
	var debugArtifacts cachedDebugArtifacts
	if flagDebugDir != "" {
		debugArtifacts.SourceFiles = make(map[string][]byte)
		debugArtifacts.GarbledFiles = make(map[string][]byte)
	}
	var buf, includeBuf bytes.Buffer
	for _, path := range paths {
		buf.Reset()
		var asmContent bytes.Buffer
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		basename := filepath.Base(path)
		defer f.Close() // in case of error
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if flagDebugDir != "" {
				asmContent.WriteString(line)
				asmContent.WriteByte('\n')
			}

			// Whole-line comments might be directives, leave them in place.
			// For example: //go:build race
			// Any other comment, including inline ones, can be discarded entirely.
			line, comment, hasComment := strings.Cut(line, "//")
			if hasComment && line == "" {
				buf.WriteString("//")
				buf.WriteString(comment)
				buf.WriteByte('\n')
				continue
			}

			// Preprocessor lines to include another file.
			// For example: #include "foo.h"
			if quoted, ok := strings.CutPrefix(line, "#include"); ok {
				quoted = strings.TrimSpace(quoted)
				includePath, err := strconv.Unquote(quoted)
				if err != nil { // note that strconv.Unquote errors do not include the input string
					return nil, fmt.Errorf("cannot unquote %q: %v", quoted, err)
				}
				newPath := newHeaderPaths[includePath]
				switch newPath {
				case missingHeader: // no need to try again
					buf.WriteString(line)
					buf.WriteByte('\n')
					continue
				case "": // first time we see this header
					includeBuf.Reset()
					content, err := os.ReadFile(includePath)
					if errors.Is(err, fs.ErrNotExist) {
						newHeaderPaths[includePath] = missingHeader
						buf.WriteString(line)
						buf.WriteByte('\n')
						continue // a header file provided by Go or the system
					} else if err != nil {
						return nil, err
					}
					basename := filepath.Base(includePath)
					if flagDebugDir != "" {
						debugArtifacts.SourceFiles[basename] = content
						if err := writeDebugDirFile(debugDirSourceSubdir, tf.curPkg, basename, content); err != nil {
							return nil, err
						}
					}
					tf.replaceAsmNames(&includeBuf, content)

					// For now, we replace `foo.h` or `dir/foo.h` with `garbled_foo.h`.
					// The different name ensures we don't use the unobfuscated file.
					// This is far from perfect, but does the job for the time being.
					// In the future, use a randomized name.
					newPath = "garbled_" + basename
					content = includeBuf.Bytes()
					if _, err := tf.writeSourceFile(basename, newPath, content); err != nil {
						return nil, err
					}
					if flagDebugDir != "" {
						debugArtifacts.GarbledFiles[basename] = content
					}
					newHeaderPaths[includePath] = newPath
				}
				buf.WriteString("#include ")
				buf.WriteString(strconv.Quote(newPath))
				buf.WriteByte('\n')
				continue
			}

			// Anything else is regular assembly; replace the names.
			tf.replaceAsmNames(&buf, []byte(line))
			buf.WriteByte('\n')
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		f.Close() // do not keep len(paths) files open
		if flagDebugDir != "" {
			content := asmContent.Bytes()
			debugArtifacts.SourceFiles[basename] = content
			if err := writeDebugDirFile(debugDirSourceSubdir, tf.curPkg, basename, content); err != nil {
				return nil, err
			}
		}

		content := buf.Bytes()

		// With assembly files, we obfuscate the filename in the temporary
		// directory, as assembly files do not support `/*line` directives.
		// TODO(mvdan): per cmd/asm/internal/lex, they do support `#line`.
		newName := hashWithPackage(tf.curPkg, basename) + ".s"
		if path, err := tf.writeSourceFile(basename, newName, content); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
		if flagDebugDir != "" {
			debugArtifacts.GarbledFiles[basename] = content
		}
	}
	if err := saveDebugArtifactsForPkg(tf.curPkg, debugCacheKindAsm, debugArtifacts); err != nil {
		return nil, err
	}

	return append(flags, newPaths...), nil
}

func (tf *transformer) replaceAsmNames(buf *bytes.Buffer, remaining []byte) {
	// We need to replace all function references with their obfuscated name
	// counterparts.
	// Luckily, all func names in Go assembly files are immediately followed
	// by the unicode "middle dot", like:
	//
	//	TEXT ·privateAdd(SB),$0-24
	//	TEXT runtime∕internal∕sys·Ctz64(SB), NOSPLIT, $0-12
	//
	// Note that import paths in assembly, like `runtime∕internal∕sys` above,
	// use Unicode periods and slashes rather than the ASCII ones used by `go list`.
	// We need to convert to ASCII to find the right package information.
	const (
		asmPeriod = '·'
		goPeriod  = '.'
		asmSlash  = '∕'
		goSlash   = '/'
	)
	asmPeriodLen := utf8.RuneLen(asmPeriod)

	for {
		periodIdx := bytes.IndexRune(remaining, asmPeriod)
		if periodIdx < 0 {
			buf.Write(remaining)
			remaining = nil
			break
		}

		// The package name ends at the first rune which cannot be part of a Go
		// import path, such as a comma or space.
		pkgStart := periodIdx
		for pkgStart >= 0 {
			c, size := utf8.DecodeLastRune(remaining[:pkgStart])
			if !unicode.IsLetter(c) && c != '_' && c != asmSlash && !unicode.IsDigit(c) {
				break
			}
			pkgStart -= size
		}
		// The package name might actually be longer, e.g:
		//
		//	JMP test∕with·many·dots∕main∕imported·PublicAdd(SB)
		//
		// We have `test∕with` so far; grab `·many·dots∕main∕imported` as well.
		pkgEnd := periodIdx
		lastAsmPeriod := -1
		for i := pkgEnd + asmPeriodLen; i <= len(remaining); {
			c, size := utf8.DecodeRune(remaining[i:])
			if c == asmPeriod {
				lastAsmPeriod = i
			} else if !unicode.IsLetter(c) && c != '_' && c != asmSlash && !unicode.IsDigit(c) {
				if lastAsmPeriod > 0 {
					pkgEnd = lastAsmPeriod
				}
				break
			}
			i += size
		}
		asmPkgPath := string(remaining[pkgStart:pkgEnd])

		// Write the bytes before our unqualified `·foo` or qualified `pkg·foo`.
		buf.Write(remaining[:pkgStart])

		// If the name was qualified, fetch the package, and write the
		// obfuscated import path if needed.
		lpkg := tf.curPkg
		if asmPkgPath != "" {
			if asmPkgPath != tf.curPkg.Name {
				goPkgPath := asmPkgPath
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmPeriod), string(goPeriod))
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmSlash), string(goSlash))
				var err error
				lpkg, err = listPackage(tf.curPkg, goPkgPath)
				if err != nil {
					panic(err) // shouldn't happen
				}
			}
			if lpkg.ToObfuscate {
				// Note that we don't need to worry about asmSlash here,
				// because our obfuscated import paths contain no slashes right now.
				buf.WriteString(lpkg.obfuscatedImportPath())
			} else {
				buf.WriteString(asmPkgPath)
			}
		}

		// Write the middle dot and advance the remaining slice.
		buf.WriteRune(asmPeriod)
		remaining = remaining[pkgEnd+asmPeriodLen:]

		// The declared name ends at the first rune which cannot be part of a Go
		// identifier, such as a comma or space.
		nameEnd := 0
		for nameEnd < len(remaining) {
			c, size := utf8.DecodeRune(remaining[nameEnd:])
			if !unicode.IsLetter(c) && c != '_' && !unicode.IsDigit(c) {
				break
			}
			nameEnd += size
		}
		name := string(remaining[:nameEnd])
		remaining = remaining[nameEnd:]

		if lpkg.ToObfuscate && !compilerIntrinsics[lpkg.ImportPath][name] {
			newName := hashWithPackage(lpkg, name)
			if flagDebug { // TODO(mvdan): remove once https://go.dev/issue/53465 if fixed
				log.Printf("asm name %q hashed with %x to %q", name, tf.curPkg.GarbleActionID, newName)
			}
			buf.WriteString(newName)
		} else {
			buf.WriteString(name)
		}
	}
}

// writeSourceFile is a mix between os.CreateTemp and os.WriteFile, as it writes a
// named source file in sharedTempDir given an input buffer.
//
// Note that the file is created under a directory tree following curPkg's
// import path, mimicking how files are laid out in modules and GOROOT.
func (tf *transformer) writeSourceFile(basename, obfuscated string, content []byte) (string, error) {
	// Uncomment for some quick debugging. Do not delete.
	// fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n%s", curPkg.ImportPath, basename, content)

	if flagDebugDir != "" {
		if err := writeDebugDirFile(debugDirGarbledSubdir, tf.curPkg, basename, content); err != nil {
			return "", err
		}
	}
	// We use the obfuscated import path to hold the temporary files.
	// Assembly files do not support line directives to set positions,
	// so the only way to not leak the import path is to replace it.
	pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedSourceDir())
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pkgDir, obfuscated)
	if err := writeFileExclusive(dstPath, content); err != nil {
		return "", err
	}
	return dstPath, nil
}
