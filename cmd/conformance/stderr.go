package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// The stderr mode answers a question the JSON mode cannot: do we *say* the same
// thing? The JSON mode only compares exit class and the line of the first
// "error:", which means every diagnostic wording, every caret, and every
// warning is unverified today.
//
// Two scores are reported separately on purpose:
//
//	msg-match  the ordered list of error:/warning: texts is identical
//	full-match the whole normalized stderr (including carets and notes) is identical
//
// Splitting them keeps "we use different words" from hiding "we point at a
// different token", which are two different pieces of work.

type sdiag struct {
	Exit    int      `json:"exit"`
	Msgs    []string `json:"msgs,omitempty"`
	FullSHA string   `json:"full_sha256,omitempty"`
	FullLen int      `json:"full_len,omitempty"`
	Full    string   `json:"full,omitempty"` // only kept when the case differs
}

type scase struct {
	File   string `json:"file"`
	Run    string `json:"run,omitempty"`
	Skip   string `json:"skip,omitempty"` // non-empty: not replayable
	Oracle sdiag  `json:"oracle"`
	Ours   sdiag  `json:"ours"`
	Status string `json:"status"` // same | msg-match | diff | skipped
}

type sreport struct {
	Tool          string         `json:"tool"`
	Oracle        string         `json:"oracle"`
	OracleVersion string         `json:"oracle_version"`
	Corpus        string         `json:"corpus"`
	Counts        map[string]int `json:"counts"`
	Cases         []scase        `json:"cases"`
}

var (
	// llvm-tblgen ends a failing run with "<argv0>: N errors."; our binary stays
	// silent. Treat the summary as noise, not as a diagnostic.
	errSummaryRe = regexp.MustCompile(`(?m)^\S*: \d+ errors?\.\n?`)
	// LLVM prefixes its own argv[0] to a few messages ("<path>/llvm-tblgen:
	// Could not open input file ..."). The harness runs two differently named
	// binaries, so the program name is an artefact of the invocation, not a
	// difference in behaviour.
	progPrefixRe = regexp.MustCompile(`(?m)^\S*tblgen: `)
	severityRe   = regexp.MustCompile(`: (error|warning): `)
)

// normalizeStderr removes the parts that are artefacts of *how* the binary was
// invoked rather than of what it found: the absolute test path (so the same
// diagnostic from two binaries compares equal) and the trailing error count.
func normalizeStderr(text, file string) string {
	if file != "" {
		text = strings.ReplaceAll(text, file, "<file>")
	}
	text = errSummaryRe.ReplaceAllString(text, "")
	text = progPrefixRe.ReplaceAllString(text, "<prog>: ")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	out := strings.Join(lines, "\n")
	return strings.TrimRight(out, "\n")
}

// stderrMsgs keeps only the diagnostic *words*, in order. Carets, notes and the
// source echo are dropped: they belong to the anchor problem, not the wording
// problem.
func stderrMsgs(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		m := severityRe.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		sev := line[m[2]:m[3]]
		out = append(out, sev+": "+line[m[1]:])
	}
	return out
}

func equalStrings(a, b []string) bool {
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

func runStderr(corpus, oracle, bin string, jobs int, timeout time.Duration, out, filter string) {
	files, err := listCorpus(corpus, filter)
	if err != nil {
		fatal(err)
	}
	if !fileExists(bin) {
		fatal(fmt.Errorf("binary %q not found; run `go build -o tblgen ./cmd/tblgen`", bin))
	}
	// This mode runs both binaries per file, so a machine-wide 30s budget that
	// is fine for the oracle alone gets exceeded by the biggest AArch64 tests
	// once every worker is busy. A timeout here is a harness artefact, not a
	// finding, so give the slow lane room.
	if timeout < 120*time.Second {
		timeout = 120 * time.Second
	}

	cases := make([]scase, len(files))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, f := range files {
		i, f := i, f
		run := runText(f)
		plan := planInput(f)
		if plan.Skip != "" {
			cases[i] = scase{File: f, Run: run, Skip: plan.Skip, Status: "skipped"}
			continue
		}
		args := append([]string{"--dump-json"}, plan.Args...)
		args = append(args, plan.Input)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			oOut, oErr, oExit := capture(timeout, oracle, args)
			_ = oOut
			sOut, sErr, sExit := capture(timeout, bin, args)
			_ = sOut
			oc := scase{File: f, Run: run}
			oc.Oracle = mkSdiag(oErr, plan.Input, oExit)
			oc.Ours = mkSdiag(sErr, plan.Input, sExit)
			oc.Status = classifyStderr(oc.Oracle, oc.Ours)
			if oc.Status != "same" {
				oc.Oracle.Full = truncate(oc.Oracle.Full, 4000)
				oc.Ours.Full = truncate(oc.Ours.Full, 4000)
			}
			cases[i] = oc
		}()
	}
	wg.Wait()

	rep := sreport{
		Tool:          bin,
		Oracle:        oracle,
		OracleVersion: oracleVersion(oracle),
		Corpus:        corpus,
		Counts:        map[string]int{},
		Cases:         cases,
	}
	for _, c := range cases {
		rep.Counts["status:"+c.Status]++
	}
	writeStderrReport(out, rep)

	// A case only enters the denominators if at least one side said something:
	// silence on both sides is agreement, not evidence.
	spoken, msg, full, skipped := 0, 0, 0, 0
	for _, c := range cases {
		if c.Status == "skipped" {
			skipped++
			continue
		}
		if len(c.Oracle.Msgs) == 0 && len(c.Ours.Msgs) == 0 {
			continue
		}
		spoken++
		if equalStrings(c.Oracle.Msgs, c.Ours.Msgs) {
			msg++
		}
		if c.Status == "same" {
			full++
		}
	}
	fmt.Printf("\nstderr: files=%d  with-diagnostics=%d  msg-match=%d/%d  full-match=%d/%d\n",
		len(cases), spoken, msg, spoken, full, spoken)
	fmt.Printf("  same %d  msg-match %d  diff %d  skipped %d\n",
		rep.Counts["status:same"], rep.Counts["status:msg-match"], rep.Counts["status:diff"], skipped)

	// Show the most common wording mismatches first: that is where a single fix
	// buys the most cases.
	var bad []scase
	for _, c := range cases {
		if c.Status == "skipped" {
			continue
		}
		if c.Status != "same" {
			bad = append(bad, c)
		}
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].File < bad[j].File })
	fmt.Printf("\nfirst mismatches (%d total):\n", len(bad))
	for i, c := range bad {
		if i >= 8 {
			break
		}
		firstDiff := ""
		for k := 0; k < len(c.Oracle.Msgs) || k < len(c.Ours.Msgs); k++ {
			var a, b string
			if k < len(c.Oracle.Msgs) {
				a = c.Oracle.Msgs[k]
			}
			if k < len(c.Ours.Msgs) {
				b = c.Ours.Msgs[k]
			}
			if a != b {
				firstDiff = fmt.Sprintf("oracle[%d]=%q  ours[%d]=%q", k, a, k, b)
				break
			}
		}
		if firstDiff == "" {
			firstDiff = "wording equal, rendering differs"
		}
		fmt.Printf("  %-46s %s\n", filepath.Base(c.File), firstDiff)
	}
}

func mkSdiag(text, file string, exit int) sdiag {
	norm := normalizeStderr(text, file)
	sum := sha256.Sum256([]byte(norm))
	return sdiag{
		Exit:    exit,
		Msgs:    stderrMsgs(norm),
		FullSHA: hex.EncodeToString(sum[:]),
		FullLen: len(norm),
		Full:    norm,
	}
}

func classifyStderr(o, s sdiag) string {
	if o.FullSHA == s.FullSHA {
		return "same"
	}
	if equalStrings(o.Msgs, s.Msgs) {
		return "msg-match"
	}
	return "diff"
}

func writeStderrReport(path string, rep sreport) {
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

var _ = runtime.NumCPU
