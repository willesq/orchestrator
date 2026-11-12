package spaces

import (
	"strings"
)

// What a call writes (spaces plan SP-042). The guard refuses writes to the
// paths orch protects (its own config and data, harness settings, git
// hooks), whether a file tool or a shell command makes them, so Targets
// reports the written paths apart from the paths a call only reads.

// writeOperands says which operands of a command that writes files are
// written: every operand, only the destination (the last operand, unless a
// target directory is given with -t), or none (only flag values below).
var writeOperands = map[string]string{
	"tee": "all", "touch": "all", "truncate": "all", "rm": "all", "rmdir": "all", "unlink": "all",
	"shred": "all", "chmod": "all", "chown": "all", "chgrp": "all", "chflags": "all", "xattr": "all",
	"patch": "all",
	// mv removes its sources, so they are written too.
	"mv": "all", "gmv": "all",
	"cp": "dest", "gcp": "dest", "install": "dest", "ln": "dest", "link": "dest", "rsync": "dest", "ditto": "dest",
	"curl": "none", "wget": "none", "unzip": "none", "tar": "none", "bsdtar": "none",
}

// writeFlags are, per command, the flags whose value is written: a target
// directory, an output file, an extraction directory.
var writeFlags = map[string][]string{
	"cp": {"-t", "--target-directory"}, "gcp": {"-t", "--target-directory"},
	"mv": {"-t", "--target-directory"}, "gmv": {"-t", "--target-directory"},
	"install": {"-t", "--target-directory"}, "ln": {"-t", "--target-directory"},
	"curl":  {"-o", "--output", "--output-dir"},
	"wget":  {"-O", "--output-document", "-P", "--directory-prefix"},
	"unzip": {"-d"},
	"patch": {"-o", "--output", "-d", "--directory"},
}

// writing reports whether paths added now are written by the call.
func (e *extractor) writingNow() bool { return e.writing > 0 }

// recordWrite lists p, an absolute path or a prefix, as written.
func (e *extractor) recordWrite(p string) {
	key := "w" + p
	if e.seen[key] {
		return
	}
	e.seen[key] = true
	e.out.Writes = append(e.out.Writes, e.res.resolve(p).path)
}

// flagValues returns the values args give the flags in names: "-o v",
// "-ov", "-xo v" (a short flag last in a cluster takes the next word, one
// inside a cluster the rest of it), "--output v" and "--output=v". rest
// are the operands left.
func flagValues(args []word, names []string) (values, rest []word) {
	short, long := map[byte]bool{}, map[string]bool{}
	for _, n := range names {
		if len(n) == 2 && n[0] == '-' {
			short[n[1]] = true
		} else {
			long[n] = true
		}
	}
	flags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.val
		next := func() {
			if i+1 < len(args) {
				i++
				values = append(values, args[i])
			}
		}
		switch {
		case v == "":
		case flags && v == "--":
			flags = false
		case flags && strings.HasPrefix(v, "--"):
			name, val, eq := strings.Cut(v, "=")
			if long[name] {
				if eq {
					values = append(values, word{val: val, partial: a.partial, glob: a.glob, lit: strings.TrimPrefix(a.lit, name+"=")})
				} else {
					next()
				}
			}
		case flags && strings.HasPrefix(v, "-") && len(v) > 1:
			for j := 1; j < len(v); j++ {
				if !short[v[j]] {
					continue
				}
				if j == len(v)-1 {
					next()
				} else {
					values = append(values, word{val: v[j+1:], partial: a.partial, glob: a.glob, lit: a.lit})
				}
				break
			}
		default:
			rest = append(rest, a)
		}
	}
	return values, rest
}

// writeArgs records the operands and flag values command name writes, as
// paths it also reaches. args are its arguments after any wrapper.
func (e *extractor) writeArgs(name string, args []word, at where) {
	mode, ok := writeOperands[name]
	switch {
	case name == "sed" || name == "gsed":
		if !hasFlag(args, "-i", "--in-place") && !hasPrefixFlag(args, "--in-place=") {
			return
		}
		mode, ok = "all", true
	case name == "dd":
		e.writing++
		for _, a := range args {
			if v, found := strings.CutPrefix(a.val, "of="); found {
				e.pathArg(word{val: v, partial: a.partial, glob: a.glob, lit: strings.TrimPrefix(a.lit, "of=")}, at)
			}
		}
		e.writing--
		return
	}
	if !ok {
		return
	}
	flags := writeFlags[name]
	if name == "tar" || name == "bsdtar" {
		flags = tarWriteFlags(args)
	}
	values, operands := flagValues(args, flags)
	e.writing++
	defer func() { e.writing-- }()
	for _, v := range values {
		e.pathArg(v, at)
	}
	switch {
	case mode == "all":
		for _, a := range operands {
			e.pathArg(a, at)
		}
	case mode == "dest" && len(values) == 0 && len(operands) > 0:
		e.pathArg(operands[len(operands)-1], at)
	case len(values) == 0 && writesHere(name, args):
		// Reason: with no output named, the file lands in the directory
		// the command runs in, which a cd may have made a protected one.
		e.pathArg(word{val: "."}, at)
	}
	if (name == "tar" || name == "bsdtar") && len(args) > 1 && !strings.HasPrefix(args[0].val, "-") &&
		strings.Contains(args[0].val, "f") && strings.ContainsAny(args[0].val, "cru") {
		// Old-style "tar czf archive ...": f's value is the next word.
		e.pathArg(args[1], at)
	}
	// Reason: a hard link to a protected file writes it under another
	// name, which no later check could trace back.
	if (name == "ln" || name == "link") && !hasFlag(args, "-s", "--symbolic") ||
		(name == "cp" || name == "gcp") && hasFlag(args, "-l", "--link") {
		for _, a := range operands {
			e.pathArg(a, at)
		}
	}
}

// writesHere reports whether a download or extraction with no output
// named writes into its working directory.
func writesHere(name string, args []word) bool {
	switch name {
	case "wget", "unzip":
		return true
	case "curl":
		return hasFlag(args, "-O", "--remote-name", "--remote-name-all")
	case "tar", "bsdtar":
		first := ""
		if len(args) > 0 && !strings.HasPrefix(args[0].val, "-") {
			first = args[0].val
		}
		return hasFlag(args, "-x", "--extract", "--get") || strings.Contains(first, "x")
	}
	return false
}

// tarWriteFlags are the flags whose value tar writes: the extraction
// directory when it extracts, the archive when it creates or appends.
func tarWriteFlags(args []word) []string {
	first := ""
	if len(args) > 0 && !strings.HasPrefix(args[0].val, "-") {
		first = args[0].val
	}
	switch {
	case hasFlag(args, "-x", "--extract", "--get") || strings.Contains(first, "x"):
		return []string{"-C", "--directory"}
	case hasFlag(args, "-c", "-r", "-u", "--create", "--append", "--update") || strings.ContainsAny(first, "cru"):
		return []string{"-f", "--file"}
	}
	return nil
}

// hasPrefixFlag reports whether an argument starts with prefix.
func hasPrefixFlag(args []word, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a.val, prefix) {
			return true
		}
	}
	return false
}

// fileToolWrites records the file a file tool writes: its path field, not
// the paths its content mentions.
func (e *extractor) fileToolWrites(name string, obj map[string]any, at where) {
	if !writeTools[name] || name == "apply_patch" {
		return
	}
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s, ok := obj[k].(string); ok && s != "" {
			for _, p := range e.absAll(at, s) {
				e.recordWrite(p)
			}
		}
	}
}
