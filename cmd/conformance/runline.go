package main

import (
	"os"
	"path/filepath"
	"strings"
)

// A file's RUN lines are the real invocation spec: llvm-tblgen with -D variants,
// -print-records instead of --dump-json, stdin instead of a path, and a
// FileCheck pipeline on the other end. The JSON compare mode throws all of that
// away and runs one fixed command per file, so ~120 negative variants that only
// exist behind -DERRORn were never executed at all.
//
// This file turns the RUN lines into invocations. Literal matching (FileCheck)
// is deliberately not emulated: the oracle binary is run with the same argv and
// its stdout/stderr/exit are compared byte for byte, which is strictly stronger.

type invocation struct {
	Argv   []string // arguments for the binary, without the program name
	Stdin  bool     // < %s
	Negate bool     // `not llvm-tblgen`
	Skip   string   // non-empty: why this invocation cannot be replayed
}

// runLines reads the file and returns its RUN commands with backslash
// continuations joined.
func runLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	var cur string
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.Index(line, "RUN:")
		if i < 0 {
			continue
		}
		text := strings.TrimSpace(line[i+4:])
		for strings.HasSuffix(text, `\`) {
			text = strings.TrimSuffix(text, `\`)
			cur += text
			text = ""
			// Continuation: the next physical line belongs to this RUN.
			break
		}
		if text == "" {
			continue
		}
		cur += text
		out = append(out, cur)
		cur = ""
	}
	// A trailing continuation with no terminator is kept as-is.
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// splitShell splits on the separators lit uses between commands, without
// splitting inside quotes.
func splitShell(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == ';':
			out = append(out, cur.String())
			cur.Reset()
		case c == '&' && i+1 < len(s) && s[i+1] == '&':
			out = append(out, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// shellFields splits one command into words, treating a pipe as a barrier: the
// FileCheck side is dropped because we compare raw bytes instead.
func shellFields(cmd string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		case c == '|':
			return out
		case c == '>' || c == '<':
			// Redirects are handled by the caller via the raw text.
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// invocationsFor turns one file's RUN lines into replayable invocations.
func invocationsFor(path string) []invocation { return invocationsForDir(path, "") }

// invocationsForDir is invocationsFor with a materialized %t tree: tests that
// start with `split-file %s %t` can only be replayed once the parts exist on
// disk. Everything else keeps %t unresolved and is reported as a skip.
func invocationsForDir(path, tdir string) []invocation {
	dir := filepath.Dir(path)
	abs := abs(path)
	subst := func(s string) (string, bool) {
		s = strings.ReplaceAll(s, "%s", abs)
		s = strings.ReplaceAll(s, "%p", dir)
		s = strings.ReplaceAll(s, "%S", dir)
		if tdir != "" {
			s = strings.ReplaceAll(s, "%t", tdir)
		}
		s = strings.ReplaceAll(s, "%%", "\x00")
		if strings.Contains(s, "%") {
			return "", false
		}
		return strings.ReplaceAll(s, "\x00", "%"), true
	}
	var out []invocation
	for _, run := range runLines(path) {
		for _, cmd := range splitShell(run) {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			inv, ok := parseInvocation(cmd, subst)
			if ok {
				out = append(out, inv)
			}
		}
	}
	return out
}

func parseInvocation(cmd string, subst func(string) (string, bool)) (invocation, bool) {
	// Redirections are shell plumbing: `2>&1` merges the streams (we capture
	// them separately), and `< %s` is fed through stdin explicitly.
	stdin := strings.Contains(cmd, "< %s") || strings.Contains(cmd, "<%s")
	clean := strings.NewReplacer("2>&1", " ", ">&2", " ", "< %s", " ", "<%s", " ").Replace(cmd)
	fields := shellFields(clean)
	if len(fields) == 0 {
		return invocation{}, false
	}
	inv := invocation{}
	if fields[0] == "not" {
		inv.Negate = true
		fields = fields[1:]
	}
	idx := -1
	for i, f := range fields {
		if strings.HasSuffix(f, "llvm-tblgen") {
			idx = i
			break
		}
	}
	if idx < 0 {
		// split-file, %python, diff, rm: nothing this binary can be run with.
		return invocation{Skip: "non-tblgen command: " + fields[0]}, true
	}
	sawFile := false
	for _, f := range fields[idx+1:] {
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, ">") || strings.HasPrefix(f, "<") {
			continue
		}
		if strings.HasPrefix(f, "-gen-") || strings.HasPrefix(f, "--gen-") {
			return invocation{Skip: "backend emitter: " + f}, true
		}
		if (strings.HasPrefix(f, "-o") || strings.HasPrefix(f, "-d")) && strings.Contains(f, "%t") {
			return invocation{Skip: "output path needs %t"}, true
		}
		if strings.HasPrefix(f, "-o") && strings.HasSuffix(f, ".inc") {
			// Redirected into a file whose content nothing compares.
			return invocation{Skip: "-o writes an include file"}, true
		}
		a, ok := subst(f)
		if !ok {
			return invocation{Skip: "unknown lit substitution: " + f}, true
		}
		inv.Argv = append(inv.Argv, a)
	}
	for _, a := range inv.Argv {
		if strings.HasSuffix(a, ".td") && !strings.HasPrefix(a, "-") {
			sawFile = true
		}
	}
	if !stdin && !sawFile {
		inv.Argv = append(inv.Argv, subFileFor(cmd, subst))
	}
	return inv, true
}

// subFileFor recovers the test path when the RUN line left it implicit; every
// caller passes the same file, so this is only used to append it.
func subFileFor(cmd string, subst func(string) (string, bool)) string {
	if v, ok := subst("%s"); ok {
		return v
	}
	return ""
}
