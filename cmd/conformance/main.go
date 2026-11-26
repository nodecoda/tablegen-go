// Differential conformance runner: our tblgen vs the reference llvm-tblgen.
//
// The reference binary is the *oracle*. A baseline run records what it does on
// every self-contained test in llvm/test/TableGen. A compare run replays the
// same inputs through our binary and classifies each case. `-mode check` is the
// ratchet: a case that matched before must still match.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultCorpus = "/home/ubuntu/llvm/llvm-project/llvm/test/TableGen"
	defaultOracle = "/home/ubuntu/llvm/builds/record-loader/bin/llvm-tblgen"
)

type runResult struct {
	Exit    int    `json:"exit"`
	Class   string `json:"class"` // valid | invalid | timeout
	OutSHA  string `json:"out_sha256,omitempty"`
	OutLen  int    `json:"out_len,omitempty"`
	ErrHead string `json:"err_head,omitempty"`
	ErrLine int    `json:"err_line,omitempty"`
}

type caseResult struct {
	File   string     `json:"file"`
	Run    string     `json:"run,omitempty"`
	Scope  string     `json:"scope"` // in-scope | skipped
	Skip   string     `json:"skip,omitempty"`
	Input  string     `json:"input,omitempty"` // what the binaries actually read
	Args   []string   `json:"args,omitempty"`
	Oracle runResult  `json:"oracle"`
	Ours   *runResult `json:"ours,omitempty"`
	Status string     `json:"status,omitempty"` // match|diff|reject-match|unsupported|timeout|crash|missing
}

type report struct {
	Tool          string         `json:"tool"`
	Oracle        string         `json:"oracle"`
	OracleVersion string         `json:"oracle_version"`
	Corpus        string         `json:"corpus"`
	Counts        map[string]int `json:"counts"`
	Cases         []caseResult   `json:"cases"`
}

func main() {
	mode := flag.String("mode", "baseline", "baseline | compare | check")
	corpus := flag.String("corpus", defaultCorpus, "directory of .td tests")
	oracle := flag.String("oracle", defaultOracle, "reference llvm-tblgen")
	bin := flag.String("bin", "./tblgen", "our binary")
	out := flag.String("out", "", "output json (default: conformance/<mode>.json)")
	jobs := flag.Int("jobs", runtime.NumCPU(), "parallel oracle invocations")
	timeout := flag.Duration("timeout", 120*time.Second, "per-file timeout")
	show := flag.String("show", "", "print both outputs for one file and exit")
	stderrMode := flag.Bool("stderr", false, "compare diagnostics text instead of JSON output")
	matrixMode := flag.Bool("matrix", false, "replay every RUN line of every test")
	filter := flag.String("filter", "", "only run files whose path contains this substring")
	useBaseline := flag.Bool("use-baseline", true, "reuse oracle results from conformance/baseline.json")
	summary := flag.Bool("summary", false, "print the combined score table and write conformance/score.json")
	flag.Parse()
	defer cleanupTemps()

	if *summary {
		runSummary(outPath(*out, "score"))
		return
	}
	if *stderrMode {
		runStderr(*corpus, *oracle, *bin, *jobs, *timeout, outPath(*out, "stderr"), *filter)
		return
	}
	if *matrixMode {
		runMatrix(*corpus, *oracle, *bin, *jobs, *timeout, outPath(*out, "matrix"), *filter)
		return
	}

	if *show != "" {
		args := append([]string{"--dump-json"}, includeArgs(abs(*show), runText(abs(*show)))...)
		args = append(args, abs(*show))
		for _, side := range []struct{ label, bin string }{{"oracle", *oracle}, {"ours", *bin}} {
			out, errOut, exit := capture(*timeout, side.bin, args)
			fmt.Printf("=== %s (%s) exit=%d ===\n%s\n", side.label, side.bin, exit, truncate(out, 2000))
			if errOut != "" {
				fmt.Printf("--- stderr ---\n%s\n", truncate(errOut, 800))
			}
		}
		return
	}

	files, err := listCorpus(*corpus, *filter)
	if err != nil {
		fatal(err)
	}
	if len(files) == 0 {
		fatal(fmt.Errorf("no .td files in %s", *corpus))
	}

	base := map[string]caseResult(nil)
	if *useBaseline && *mode != "baseline" {
		base = loadBaseline("conformance/baseline.json", *oracle)
	}
	cases := make([]caseResult, len(files))
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	reused := 0
	for i, f := range files {
		i, f := i, f
		plan := planInput(f)
		cases[i] = caseResult{File: f, Run: runText(f), Input: plan.Input, Args: plan.Args}
		if plan.Skip != "" {
			cases[i].Scope = "skipped"
			cases[i].Skip = plan.Skip
			continue
		}
		cases[i].Scope = "in-scope"
		if b, ok := base[f]; ok && samePlan(b, cases[i]) {
			cases[i].Oracle = b.Oracle
			reused++
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cmdArgs := append([]string{"--dump-json"}, plan.Args...)
			cmdArgs = append(cmdArgs, plan.Input)
			cases[i].Oracle = runOne(*timeout, *oracle, cmdArgs)
		}()
	}
	wg.Wait()
	if *mode != "baseline" && *useBaseline {
		fmt.Printf("oracle: reused %d/%d from baseline, ran %d\n", reused, len(files), len(files)-reused)
	}

	if *mode == "baseline" {
		write(outPath(*out, *mode), buildReport(*oracle, *corpus, cases, nil))
		summarize(cases, "baseline")
		return
	}

	// compare / check: replay through our binary. Parallel like the oracle
	// phase above: some corpus files expand to tens of MB of JSON (Target.td
	// pulls in the whole target description), and running 392 of those
	// serially made a full compare take tens of minutes.
	binMissing := !fileExists(*bin)
	if !binMissing {
		var wg sync.WaitGroup
		sem := make(chan struct{}, *jobs)
		for i := range cases {
			if cases[i].Scope != "in-scope" {
				continue
			}
			i := i
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				cmdArgs := append([]string{"--dump-json"}, cases[i].Args...)
				cmdArgs = append(cmdArgs, cases[i].Input)
				cases[i].Ours = ptr(runOne(*timeout, *bin, cmdArgs))
				cases[i].Status = classify(cases[i].Oracle, *cases[i].Ours)
			}()
		}
		wg.Wait()
	} else {
		for i := range cases {
			if cases[i].Scope == "in-scope" {
				cases[i].Status = "missing"
			}
		}
	}

	var prev *report
	if *mode == "check" {
		prev = readReport(outPath(*out, "compare"))
	}
	rep := buildReport(*oracle, *corpus, cases, ptr(*bin))
	write(outPath(*out, *mode), rep)
	summarize(cases, *mode)

	if binMissing {
		fatal(fmt.Errorf("binary %q not found; run `go build -o tblgen ./cmd/tblgen`", *bin))
	}
	for _, c := range cases {
		switch c.Status {
		case "crash":
			fatal(fmt.Errorf("crash on %s", c.File))
		case "timeout":
			fatal(fmt.Errorf("timeout on %s after %s (lower -jobs or raise -timeout)", c.File, *timeout))
		}
	}
	if *mode == "check" && prev != nil {
		if regressed := ratchet(*prev, rep); len(regressed) > 0 {
			fmt.Fprintf(os.Stderr, "\nratchet regressions (%d):\n", len(regressed))
			for _, f := range regressed {
				fmt.Fprintf(os.Stderr, "  - %s\n", f)
			}
			os.Exit(1)
		}
		fmt.Println("\nratchet: no regressions")
	}
}

func outPath(flagVal, mode string) string {
	if flagVal != "" {
		return flagVal
	}
	switch mode {
	case "baseline":
		return "conformance/baseline.json"
	case "stderr":
		return "conformance/stderr.json"
	case "matrix":
		return "conformance/matrix.json"
	case "score":
		return "conformance/score.json"
	}
	return "conformance/status.json"
}

func ptr[T any](v T) *T { return &v }

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "conformance:", err)
	os.Exit(1)
}

// listCorpusRec walks subdirectories too. The per-backend tests live there
// (GlobalISelEmitter, DecoderEmitter, ...), and matrix mode wants the full RUN
// surface even though the JSON compare keeps the flat top-level corpus.
func listCorpusRec(dir string, filter string) ([]string, error) {
	var fs []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".td") {
			return nil
		}
		fs = append(fs, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, f := range fs {
		if filter != "" && !strings.Contains(f, filter) {
			continue
		}
		kept = append(kept, f)
	}
	sort.Strings(kept)
	return kept, nil
}

func listCorpus(dir string, filter ...string) ([]string, error) {
	fs, err := filepath.Glob(filepath.Join(dir, "*.td"))
	if err != nil {
		return nil, err
	}
	if len(filter) > 0 && filter[0] != "" {
		var kept []string
		for _, f := range fs {
			if strings.Contains(f, filter[0]) {
				kept = append(kept, f)
			}
		}
		fs = kept
	}
	sort.Strings(fs)
	return fs, nil
}

// runText joins every RUN line: a file may have several, and only one of them
// may need an include path. Looking at just the first one misclassifies files.
func runText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "RUN:"); i >= 0 {
			out = append(out, strings.TrimSpace(line[i+4:]))
		}
	}
	return strings.Join(out, " ; ")
}

// includeArgs extracts -I directories from the RUN lines and substitutes the
// lit path variables that are statically known (%p and %S both mean the
// directory of the test file). Directories that need %t (a per-test temporary
// directory created by split-file) cannot be reconstructed after the fact, so
// they are dropped: both binaries then see the same missing include and the
// comparison stays fair.
func includeArgs(file, run string) []string {
	dir := filepath.Dir(file)
	subst := func(s string) (string, bool) {
		s = strings.ReplaceAll(s, "%p", dir)
		s = strings.ReplaceAll(s, "%S", dir)
		s = strings.ReplaceAll(s, "%s", file)
		s = strings.ReplaceAll(s, "%%", "%")
		if strings.Contains(s, "%") {
			return "", false
		}
		return filepath.Clean(s), true
	}
	var out []string
	seen := map[string]bool{}
	fields := strings.Fields(run)
	for i := 0; i < len(fields); i++ {
		var raw string
		switch {
		case fields[i] == "-I" && i+1 < len(fields):
			i++
			raw = fields[i]
		case strings.HasPrefix(fields[i], "-I") && len(fields[i]) > 2:
			raw = fields[i][2:]
		default:
			continue
		}
		d, ok := subst(strings.Trim(raw, "\"'"))
		if !ok || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, "-I", d)
	}
	return out
}

func capture(timeout time.Duration, bin string, args []string) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
			stderr.WriteString(err.Error())
		}
	}
	return stdout.String(), stderr.String(), exit
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

func runOne(timeout time.Duration, bin string, args []string) runResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	r := runResult{Exit: 0, Class: "valid"}
	out := stdout.String()
	r.OutLen = len(out)
	sum := sha256.Sum256([]byte(out))
	r.OutSHA = hex.EncodeToString(sum[:])
	r.ErrHead = firstNonEmptyLine(stderr.String())
	r.ErrLine = errorLine(stderr.String())

	if ctx.Err() == context.DeadlineExceeded {
		r.Class = "timeout"
		r.Exit = -1
		return r
	}
	if err != nil {
		r.Class = "invalid"
		if ee, ok := err.(*exec.ExitError); ok {
			r.Exit = ee.ExitCode()
		} else {
			r.Exit = -1
			r.ErrHead = err.Error()
		}
		return r
	}
	if !json.Valid([]byte(out)) {
		r.Class = "invalid"
		r.ErrHead = "stdout is not valid JSON (exit 0)"
	}
	return r
}

var diagLineRe = regexp.MustCompile(`:(\d+):(\d+):`)

// errorLine extracts the line number of the first diagnostic that says "error:".
// Matching it makes reject-match mean "rejected the same construct", not merely
// "rejected something".
func errorLine(stderr string) int {
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "error:") {
			continue
		}
		if m := diagLineRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// stripProgram removes the "<argv0>: " prefix reportError adds. Both binaries
// print their own path there, so it carries no conformance information.
func stripProgram(s string) string {
	if i := strings.Index(s, ": "); i > 0 && strings.Contains(s[:i], "tblgen") {
		return s[i+2:]
	}
	return s
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if len(t) > 200 {
				t = t[:200] + "..."
			}
			return t
		}
	}
	return ""
}

func classify(oracle, ours runResult) string {
	switch {
	case ours.Exit == 2:
		return "unsupported"
	case ours.Class == "timeout":
		return "timeout"
	case ours.Exit == -1:
		return "crash"
	case oracle.Class == "valid" && ours.Class == "valid":
		if oracle.OutSHA == ours.OutSHA {
			return "match"
		}
		return "diff"
	case oracle.Class == "invalid" && ours.Class == "invalid" && oracle.ErrLine == ours.ErrLine && oracle.ErrLine != 0:
		return "reject-match"
	case oracle.Class == "invalid" && ours.Class == "invalid" &&
		oracle.ErrLine == 0 && ours.ErrLine == 0 &&
		stripProgram(oracle.ErrHead) == stripProgram(ours.ErrHead):
		// No source location on either side: compare the message itself.
		// The program name is part of the line ("/path/llvm-tblgen: ..."),
		// so it cannot match across two different binaries.
		return "reject-match"
	default:
		return "diff"
	}
}

func buildReport(oracle, corpus string, cases []caseResult, bin *string) report {
	rep := report{
		Oracle:        oracle,
		OracleVersion: oracleVersion(oracle),
		Corpus:        corpus,
		Counts:        map[string]int{},
		Cases:         cases,
	}
	if bin != nil {
		rep.Tool = *bin
	}
	for _, c := range cases {
		rep.Counts["scope:"+c.Scope]++
		if c.Oracle.Class != "" {
			rep.Counts["oracle:"+c.Oracle.Class]++
		}
		if c.Status != "" {
			rep.Counts["status:"+c.Status]++
		}
	}
	return rep
}

func oracleVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) >= 2 {
		return strings.TrimSpace(lines[1])
	}
	return strings.TrimSpace(string(out))
}

func write(path string, rep report) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal(err)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fatal(err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s\n", path)
}

func readReport(path string) *report {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: no previous %s, treating ratchet as empty\n", path)
		return nil
	}
	var rep report
	if err := json.Unmarshal(b, &rep); err != nil {
		fatal(fmt.Errorf("parse %s: %w", path, err))
	}
	return &rep
}

func ratchet(prev, cur report) []string {
	now := map[string]string{}
	for _, c := range cur.Cases {
		now[c.File] = c.Status
	}
	var bad []string
	for _, c := range prev.Cases {
		if c.Status == "match" && now[c.File] != "match" {
			bad = append(bad, fmt.Sprintf("%s (%s -> %s)", c.File, c.Status, now[c.File]))
		}
	}
	return bad
}

func summarize(cases []caseResult, mode string) {
	counts := map[string]int{}
	scope := map[string]int{}
	for _, c := range cases {
		scope[c.Scope]++
		if c.Status != "" {
			counts[c.Status]++
		}
	}
	fmt.Printf("\nmode=%s  files=%d  in-scope=%d  skipped=%d\n",
		mode, len(cases), scope["in-scope"], scope["skipped"])
	if reasons := skipReasons(cases); len(reasons) > 0 {
		for _, r := range reasons {
			fmt.Printf("  skip: %s\n", r)
		}
	}
	if len(counts) == 0 {
		return
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-14s %d\n", k, counts[k])
	}
}

// inputPlan is what the one-invocation-per-file modes hand to both binaries.
// Most tests need nothing beyond their own path and the -I flags on their RUN
// lines. Tests that build their inputs with `split-file` need the container
// materialized and the primary input is the part the first tblgen RUN line
// reads, not the container itself.
type inputPlan struct {
	Input string
	Args  []string
	Skip  string
}

func planInput(file string) inputPlan {
	run := runText(file)
	if !strings.Contains(run, "split-file") {
		return inputPlan{Input: abs(file), Args: includeArgs(abs(file), run)}
	}
	dir, err := materializeSplit(abs(file))
	if err != nil {
		return inputPlan{Skip: "split-file: " + err.Error()}
	}
	keepTemp(dir)
	for _, inv := range invocationsForDir(file, dir) {
		if inv.Skip != "" || len(inv.Argv) == 0 {
			continue
		}
		input := ""
		for _, a := range inv.Argv {
			if strings.HasSuffix(a, ".td") && !strings.HasPrefix(a, "-") {
				input = a
			}
		}
		if input == "" {
			continue
		}
		return inputPlan{Input: input, Args: includeDirs(inv.Argv)}
	}
	return inputPlan{Skip: "split-file: no replayable invocation"}
}

// includeDirs keeps only the -I pairs of an already-substituted argv.
func includeDirs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "-I" && i+1 < len(argv):
			i++
			out = append(out, "-I", argv[i])
		case strings.HasPrefix(argv[i], "-I") && len(argv[i]) > 2:
			out = append(out, "-I", argv[i][2:])
		}
	}
	return out
}

var (
	tmpMu   sync.Mutex
	tmpDirs []string
)

func keepTemp(dir string) {
	tmpMu.Lock()
	tmpDirs = append(tmpDirs, dir)
	tmpMu.Unlock()
}

// cleanupTemps removes every split-file tree. The trees must outlive both the
// oracle and the ours phase, so their lifetime is the process, not the case.
func cleanupTemps() {
	tmpMu.Lock()
	dirs := tmpDirs
	tmpDirs = nil
	tmpMu.Unlock()
	for _, d := range dirs {
		os.RemoveAll(d)
	}
}

// loadBaseline indexes the recorded oracle results. The cache is only trusted
// when it was produced by the same oracle binary and the same input plan, which
// is what samePlan checks per case.
func loadBaseline(path, oracle string) map[string]caseResult {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rep report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil
	}
	if v := oracleVersion(oracle); rep.OracleVersion != "" && v != "" && rep.OracleVersion != v {
		fmt.Fprintf(os.Stderr, "conformance: baseline oracle %q != current %q; not reusing it\n", rep.OracleVersion, v)
		return nil
	}
	m := make(map[string]caseResult, len(rep.Cases))
	for _, c := range rep.Cases {
		m[c.File] = c
	}
	return m
}

func samePlan(b, cur caseResult) bool {
	if b.Run != cur.Run || !equalArgs(b.Args, cur.Args) {
		return false
	}
	input := b.Input
	if input == "" {
		input = abs(b.File)
	}
	return input == cur.Input
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func skipReasons(cases []caseResult) []string {
	counts := map[string]int{}
	for _, c := range cases {
		if c.Skip != "" {
			counts[c.Skip]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s (%d)", k, counts[k]))
	}
	return out
}
