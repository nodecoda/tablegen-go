// tblgen is a Go implementation of LLVM's TableGen language core: lexer,
// parser, record graph, and the record-level backends (--dump-json,
// --print-records, -o/-d). Target instruction-selection emitters are out of
// scope and exit 2 with E_UNSUPPORTED — loudly, never silently.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
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
		fmt.Fprintf(os.Stderr, "tblgen: %v\n", err)
		os.Exit(1)
	}
	if *printExpandedSource {
		toks, diags := syntax.TokenizeWithDirs(inputName, string(src), macros, includes)
		cache := newSourceCache(inputName, []byte(src))
		if err := printExpanded(os.Stdout, toks, cache); err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: %v\n", err)
			os.Exit(1)
		}
		for _, d := range diags {
			fmt.Fprint(os.Stderr, d.Render(string(src)))
		}
		os.Exit(diagExit(diags))
	}
	popts := syntax.Options{NoWarnUnusedTArgs: *noWarnUnusedTArgs}
	file, diags := syntax.ParseWith(inputName, string(src), macros, includes, nil, popts)
	// A statement-level foreach whose record names are built by bang
	// expressions is opaque to the parser. LLVM's parser resolves the loop
	// immediately, so later statements see those records; ours can only learn
	// them by lowering once, then parsing again with the names installed at
	// the end of the same loop.
	if file.DynLoopNames && hasUndefinedName(diags) {
		if k, _ := record.Lower(file, files[0]); len(k.LoopDefs()) > 0 {
			known := map[int][]syntax.KnownDef{}
			for seq, defs := range k.LoopDefs() {
				for _, d := range defs {
					known[seq] = append(known[seq], syntax.KnownDef{Name: d.Name, Parents: d.Parents})
				}
			}
			if f2, d2 := syntax.ParseWith(inputName, string(src), macros, includes, known, popts); countUndefined(d2) < countUndefined(diags) {
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
		fmt.Fprint(os.Stderr, d.Render(string(src)))
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
		fmt.Fprint(os.Stderr, d.Render(string(src)))
	}
	if code := diagExit(lowerDiags); code != 0 {
		os.Exit(code)
	}

	if *nullBackend {
		return
	}
	if *dumpJSON {
		out, err := backend.JSON(keeper)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(out)
		return
	}
	if *detailedRecords {
		unsupported("-print-detailed-records")
	}
	if *printRecords || !*dumpJSON {
		// --print-records is also llvm-tblgen's default action.
		out, err := backend.PrintRecords(keeper)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
			os.Exit(1)
		}
		emit(out, *outFile)
		writeDepfile(*outFile, inputName, *depFile)
		return
	}
}

// writeDepfile emits the make-style dependency file `-d` asks for: the output
// target followed by the primary input. With stdin there is no input path, but
// the target must still appear (an empty depfile is treated as missing).
func writeDepfile(outPath, inputName, depPath string) {
	if depPath == "" {
		return
	}
	target := outPath
	if target == "" || target == "-" {
		target = "-"
	}
	line := target + ":"
	if inputName != "" && inputName != "-" {
		line += " " + inputName
	}
	if err := os.WriteFile(depPath, []byte(line+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "tblgen: error: %v\n", err)
		os.Exit(1)
	}
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
		fmt.Fprint(os.Stderr, d.Render(string(src)))
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
