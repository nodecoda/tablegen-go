// tblgen is a Go implementation of LLVM's TableGen language core: lexer,
// parser, record graph, and the record-level backends (--dump-json,
// --print-records, -o/-d). Target instruction-selection emitters are out of
// scope and exit 2 with E_UNSUPPORTED — loudly, never silently.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"tblgen/internal/backend"
	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

const version = "0.5.0-dev"

type stringList []string

func (m *stringList) String() string     { return fmt.Sprint(*m) }
func (m *stringList) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var macros stringList
	var includes stringList
	dumpTokens := flag.Bool("dump-tokens", false, "print the token stream (development aid, not an LLVM action)")
	dumpAST := flag.Bool("dump-ast", false, "print the parsed AST (development aid, not an LLVM action)")
	dumpJSON := flag.Bool("dump-json", false, "dump all records as JSON")
	printExpandedSource := flag.Bool("E", false, "print the preprocessed token stream")
	printRecords := flag.Bool("print-records", false, "print all records (the default action)")
	detailedRecords := flag.Bool("print-detailed-records", false, "print the detailed records report")
	printSets := flag.Bool("print-sets", false, "print expanded sets for testing DAG exprs")
	nullBackend := flag.Bool("null-backend", false, "resolve records and emit nothing")
	noWarnUnusedTArgs := flag.Bool("no-warn-on-unused-template-args", false,
		"suppress unused template argument warnings")
	outFile := flag.String("o", "", "write output to this file instead of stdout")
	depFile := flag.String("d", "", "write a make-style dependency file")
	showVersion := flag.Bool("version", false, "print version")
	flag.Var(&macros, "D", "define a preprocessor macro")
	flag.Var(&includes, "I", "add a directory to the include search path")
	flag.Usage = usage
	// LLVM's cl parser accepts both `-D name` and `-Dname`; Go's flag package
	// only accepts the first form. Every RUN line in the test suite uses the
	// joined form, so normalize it before parsing.
	os.Args = append([]string{os.Args[0]}, reorderArgs(normalizeArgs(os.Args[1:]))...)
	flag.Parse()

	for _, m := range macros {
		if !validMacroName(m) {
			// TGLexer.cpp:84-90 checks the name before parsing starts and
			// reports without a source location, so this cannot go through the
			// diagnostic renderer.
			fmt.Fprintf(os.Stderr, "error: invalid macro name `%s` specified on command line\n", m)
			os.Exit(1)
		}
	}

	if *showVersion {
		fmt.Printf("tblgen %s\n", version)
		fmt.Println("scope:    TableGen language core + record-level backends (--dump-json, --print-records)")
		fmt.Println("oracle:   llvm-tblgen 24.0.0git (--dump-json)")
		return
	}

	files := flag.Args()
	if *dumpTokens {
		if len(files) != 1 {
			fmt.Fprintln(os.Stderr, "tblgen: --dump-tokens needs exactly one input file")
			os.Exit(64)
		}
		os.Exit(dumpTokensTo(files[0], macros, includes))
	}
	if len(files) > 1 {
		// LLVM's cl parser wording and exit code; 64 would diverge on every
		// file whose RUN line we mis-split.
		fmt.Fprintf(os.Stderr, "tblgen: Too many positional arguments specified!\n"+
			"Can specify at most 1 positional argument: See tblgen --help.\n")
		os.Exit(1)
	}
	// Like llvm-tblgen, no file and `-` both mean standard input.
	inputName := ""
	if len(files) == 1 {
		inputName = files[0]
	}
	if inputName == "-" {
		inputName = ""
	}
	var src []byte
	var err error
	if inputName == "" {
		src, err = io.ReadAll(os.Stdin)
		inputName = "-"
	} else {
		src, err = os.ReadFile(inputName)
	}
	if err != nil {
		// TableGenMain's reportError: "<argv0>: Could not open input file
		// '<name>': <message>" (Main.cpp:245). The OS text is capitalized
		// there, so match that too.
		fmt.Fprintf(os.Stderr, "%s: Could not open input file '%s': %s\n",
			os.Args[0], inputName, osErrText(err))
		os.Exit(1)
	}
	if *printExpandedSource {
		toks, diags, sources := syntax.TokenizeWithDeps(inputName, string(src), macros, includes)
		for _, d := range diags {
			fmt.Fprint(os.Stderr, renderDiag(d, string(src), inputName))
		}
		if code := diagExit(diags); code != 0 {
			os.Exit(code)
		}
		// Main.cpp:263-271 writes the depfile before the output, and both
		// only after the lexer has run without errors.
		writeDepfile(*outFile, inputName, sources, *depFile)
		cache := newSourceCache(inputName, []byte(src))
		var buf bytes.Buffer
		if err := printExpanded(&buf, toks, cache); err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: %v\n", err)
			os.Exit(1)
		}
		emit(buf.Bytes(), *outFile)
		return
	}
	popts := syntax.Options{NoWarnUnusedTArgs: *noWarnUnusedTArgs}
	// The retry below re-parses the same file with loop names installed; the
	// tokens are read-only, so lex once and parse twice.
	tokens := syntax.TokenizeFile(inputName, string(src), macros, includes)
	file, diags := tokens.Parse(inputName, string(src), includes, nil, popts)
	// A statement-level foreach whose record names are built by bang
	// expressions is opaque to the parser. LLVM's parser resolves the loop
	// immediately, so later statements see those records; ours can only learn
	// them by lowering once, then parsing again with the names installed at
	// the end of the same loop.
	if file.DynLoopNames && hasUndefinedName(diags) {
		k, _ := record.LowerNames(file, files[0])
		if len(k.LoopDefs()) > 0 {
			known := map[int][]syntax.KnownDef{}
			for seq, defs := range k.LoopDefs() {
				for _, d := range defs {
					known[seq] = append(known[seq], syntax.KnownDef{Name: d.Name, Parents: d.Parents})
				}
			}
			if f2, d2 := tokens.Parse(inputName, string(src), includes, known, popts); countUndefined(d2) < countUndefined(diags) {
				file, diags = f2, d2
			}
		}
	}
	softErr := false
	defer func() {
		// PrintError inside a folding operator reports and keeps going, but
		// still makes llvm-tblgen exit non-zero (Main.cpp counts every error).
		if softErr {
			os.Exit(1)
		}
	}()
	for _, d := range diags {
		if d.Soft {
			softErr = true
		}
		fmt.Fprint(os.Stderr, renderDiag(d, string(src), inputName))
	}
	if code := diagExit(diags); code != 0 {
		os.Exit(code)
	}

	if *dumpAST {
		fmt.Print(syntax.DumpFile(file))
		return
	}

	keeper, lowerDiags := record.Lower(file, inputName)
	for _, d := range lowerDiags {
		if d.Soft {
			softErr = true
		}
		fmt.Fprint(os.Stderr, renderDiag(d, string(src), inputName))
	}
	if code := diagExit(lowerDiags); code != 0 {
		os.Exit(code)
	}
	// Main.cpp:292 returns right after parsing when ErrorsPrinted > 0: a
	// PrintError from a folding operator still suppresses the backend output
	// (find.td -DERROR1 prints neither the classes nor the defs).
	if softErr {
		os.Exit(1)
	}

	if *nullBackend {
		return
	}
	if *printSets {
		out, err := backend.PrintSets(keeper)
		if err != nil {
			// SetTheory's operators call PrintFatalError, which exits 1.
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		writeDepfile(*outFile, inputName, file.Sources, *depFile)
		emit(out, *outFile)
		return
	}
	if *dumpJSON {
		out, err := backend.JSON(keeper)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
			os.Exit(1)
		}
		writeDepfile(*outFile, inputName, file.Sources, *depFile)
		emit(out, *outFile)
		return
	}
	if *detailedRecords {
		out, err := backend.Detailed(keeper)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
			os.Exit(1)
		}
		writeDepfile(*outFile, inputName, file.Sources, *depFile)
		emit(out, *outFile)
		return
	}
	if *printRecords || !*dumpJSON {
		// --print-records is also llvm-tblgen's default action.
		out, err := backend.PrintRecords(keeper)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
			os.Exit(1)
		}
		writeDepfile(*outFile, inputName, file.Sources, *depFile)
		emit(out, *outFile)
		return
	}
}

// osErrText renders an OS error the way std::error_code::message does:
// capitalized, without the "open <path>: " prefix os.PathError adds.
func osErrText(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		err = pe.Err
	}
	s := err.Error()
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

// writeDepfile emits the make-style dependency file `-d` asks for: the output
// target, the primary input, and every include the lexer opened. This is
// Main.cpp:184-217 createDependencyFile: dependencies are sorted and
// deduplicated like the std::set there, -d without -o is an error, and names
// go through escapeDependencyFilename (Main.cpp:142).
func writeDepfile(outPath, inputName string, sources []string, depPath string) {
	if depPath == "" {
		return
	}
	if outPath == "" || outPath == "-" {
		fmt.Fprintf(os.Stderr, "%s: the option -d must be used together with -o\n", os.Args[0])
		os.Exit(1)
	}
	var b strings.Builder
	b.WriteString(escapeDependencyFilename(outPath))
	b.WriteByte(':')
	if inputName != "" && inputName != "-" {
		b.WriteByte(' ')
		b.WriteString(escapeDependencyFilename(inputName))
	}
	seen := make(map[string]bool, len(sources))
	deps := make([]string, 0, len(sources))
	for _, s := range sources {
		if !seen[s] {
			seen[s] = true
			deps = append(deps, s)
		}
	}
	sort.Strings(deps)
	for _, d := range deps {
		b.WriteByte(' ')
		b.WriteString(escapeDependencyFilename(d))
	}
	b.WriteByte('\n')
	if err := os.WriteFile(depPath, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%s: error opening %s:%s\n", os.Args[0], depPath, osErrText(err))
		os.Exit(1)
	}
}

// escapeDependencyFilename mirrors Main.cpp:142 for non-Windows: `#` becomes
// `\#`, a space gains a preceding `\` plus one more for each backslash that
// already precedes it, and `$` is doubled.
func escapeDependencyFilename(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '#':
			b.WriteByte('\\')
		case ' ':
			b.WriteByte('\\')
			for j := i; j > 0 && name[j-1] == '\\'; j-- {
				b.WriteByte('\\')
			}
		case '$':
			b.WriteByte('$')
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// emit writes an action's output, honouring -o the way llvm-tblgen does.
// `-o -` means standard output.
func emit(out []byte, path string) {
	if path == "" || path == "-" {
		os.Stdout.Write(out)
		return
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
		os.Exit(1)
	}
}

func hasUndefinedName(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == "E_PARSE_VAR_UNDEFINED" {
			return true
		}
	}
	return false
}

func countUndefined(diags []diag.Diagnostic) int {
	n := 0
	for _, d := range diags {
		if d.Code == "E_PARSE_VAR_UNDEFINED" {
			n++
		}
	}
	return n
}

// diagExit maps diagnostics to LLVM-like exit codes: 1 for a rejected input,
// 2 for a feature we have not implemented. Unsupported wins, because pretending
// a feature worked is worse than any parse error.
func diagExit(diags []diag.Diagnostic) int {
	code := 0
	for _, d := range diags {
		if d.Severity != diag.Error || d.Soft {
			continue
		}
		if strings.HasPrefix(d.Code, "E_UNSUPPORTED") {
			return 2
		}
		code = 1
	}
	return code
}

// reorderArgs moves options ahead of positional arguments. Go's flag package
// stops at the first non-flag, while LLVM's cl parser accepts `file.td -DNAME`.
// Value-taking options are moved together with their value.
func reorderArgs(args []string) []string {
	takesValue := map[string]bool{"-D": true, "-I": true, "-o": true, "-d": true}
	var opts, files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || a == "" || a[0] != '-' {
			files = append(files, a)
			continue
		}
		opts = append(opts, a)
		if takesValue[a] && i+1 < len(args) {
			i++
			opts = append(opts, args[i])
		}
	}
	return append(opts, files...)
}

// normalizeArgs rewrites -XYYY into -X YYY for the joined flags LLVM's cl
// parser accepts (-DNAME, -Ipath).
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if len(a) > 2 && a[0] == '-' && (a[1] == 'D' || a[1] == 'I') && a[2] != '=' && !strings.HasPrefix(a, "--") {
			out = append(out, a[:2], a[2:])
			continue
		}
		out = append(out, a)
	}
	return out
}

// unsupported is the contract for everything past M2: fail loudly with a stable
// code and exit 2, never a silently wrong record.
func unsupported(action string) {
	fmt.Fprintf(os.Stderr, "tblgen: error: E_UNSUPPORTED: %s is not implemented yet (record-level core only; target emitters are out of scope)\n", action)
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tblgen [action] [-D name]... [-I dir]... <file.td>")
	fmt.Fprintln(os.Stderr, "\nactions: --dump-tokens --dump-ast (development aids)")
	fmt.Fprintln(os.Stderr, "         --dump-json --print-records --null-backend (M5)")
}

func dumpTokensTo(path string, macros, includes []string) int {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tblgen: %v\n", err)
		return 1
	}
	toks, diags := syntax.TokenizeWithDirs(path, string(src), macros, includes)
	for _, t := range toks {
		if t.Kind == syntax.Eof {
			break
		}
		text := t.Text
		if text == "" {
			text = t.Kind.String()
		}
		fmt.Printf("%d:%d\t%s\t%s\n", t.Span.Start.Line, t.Span.Start.Col, t.Kind, text)
	}
	for _, d := range diags {
		fmt.Fprint(os.Stderr, renderDiag(d, string(src), path))
	}
	return diagExit(diags)
}

// validMacroName is TGLexer::lexMacroName: [A-Za-z_][A-Za-z0-9_]* and nothing
// else - in particular no `=value` suffix.
func validMacroName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// renderDiag gives the diagnostic renderer access to included files: a
// diagnostic can point into an include, and printing the main file's line there
// would show the wrong source text.
func renderDiag(d diag.Diagnostic, src, inputName string) string {
	cache := map[string]string{}
	return d.RenderWith(src, func(file string) (string, bool) {
		if file == inputName || file == "-" || file == "" {
			return src, true
		}
		if t, ok := cache[file]; ok {
			return t, true
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", false
		}
		cache[file] = string(b)
		return string(b), true
	})
}
