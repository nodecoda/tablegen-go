package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Matrix mode replays every RUN line of every test instead of one fixed
// command per file. It answers "does our binary behave the same under the
// invocations LLVM actually tests", which is where the -DERRORn negative
// variants and the -print-records/stdin paths live.

type mr struct {
	Exit int    `json:"exit"`
	Out  string `json:"out_sha256,omitempty"`
	Err  string `json:"err_sha256,omitempty"`
	Msgs []string
	Head string `json:"err_head,omitempty"`
	Len  int    `json:"out_len,omitempty"`
}

type mcase struct {
	File    string   `json:"file"`
	Argv    []string `json:"argv,omitempty"`
	Negate  bool     `json:"negate,omitempty"`
	Stdin   bool     `json:"stdin,omitempty"`
	Skip    string   `json:"skip,omitempty"`
	Oracle  mr       `json:"oracle,omitempty"`
	Ours    mr       `json:"ours,omitempty"`
	Status  string   `json:"status"`
	OutLen  int      `json:"out_len,omitempty"`
	ErrText string   `json:"err_text,omitempty"`
}

type mreport struct {
	Tool          string         `json:"tool"`
	Oracle        string         `json:"oracle"`
	OracleVersion string         `json:"oracle_version"`
	Corpus        string         `json:"corpus"`
	Counts        map[string]int `json:"counts"`
	Skips         map[string]int `json:"skips"`
	Cases         []mcase        `json:"cases"`
}

func runMatrix(corpus, oracle, bin string, jobs int, timeout time.Duration, out, filter string) {
	files, err := listCorpusRec(corpus, filter)
	if err != nil {
		fatal(err)
	}
	if !fileExists(bin) {
		fatal(fmt.Errorf("binary %q not found; run `go build -o tblgen ./cmd/tblgen`", bin))
	}
	if timeout < 120*time.Second {
		timeout = 120 * time.Second
	}

	type job struct {
		file string
		inv  invocation
	}
	var jobsList []job
	seen := map[string]bool{}
	for _, f := range files {
		for _, inv := range invocationsFor(f) {
			key := f + "\x00" + strings.Join(inv.Argv, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			jobsList = append(jobsList, job{f, inv})
		}
	}

	cases := make([]mcase, len(jobsList))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, j := range jobsList {
		i, j := i, j
		if j.inv.Skip != "" {
			cases[i] = mcase{File: j.file, Argv: j.inv.Argv, Skip: j.inv.Skip, Status: "skipped"}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var stdin []byte
			if j.inv.Stdin {
				stdin, _ = os.ReadFile(abs(j.file))
			}
			oOut, oErr, oExit := captureInput(timeout, oracle, j.inv.Argv, stdin)
			sOut, sErr, sExit := captureInput(timeout, bin, j.inv.Argv, stdin)
			c := mcase{File: j.file, Argv: j.inv.Argv, Negate: j.inv.Negate, Stdin: j.inv.Stdin}
			c.Oracle = mkMR(oOut, oErr, oExit, abs(j.file))
			c.Ours = mkMR(sOut, sErr, sExit, abs(j.file))
			c.Status = classifyMatrix(c.Oracle, c.Ours)
			c.OutLen = c.Oracle.Len
			if c.Status != "same" {
				c.ErrText = truncate(strings.TrimSpace(normalizeStderr(sErr, abs(j.file))), 600)
			}
			cases[i] = c
		}()
	}
	wg.Wait()

	rep := mreport{
		Tool:          bin,
		Oracle:        oracle,
		OracleVersion: oracleVersion(oracle),
		Corpus:        corpus,
		Counts:        map[string]int{},
		Skips:         map[string]int{},
		Cases:         cases,
	}
	for _, c := range cases {
		rep.Counts["status:"+c.Status]++
		if c.Skip != "" {
			rep.Skips[c.Skip]++
		}
	}
	writeMatrix(out, rep)

	run := rep.Counts["status:same"] + rep.Counts["status:out-diff"] +
		rep.Counts["status:err-wording"] + rep.Counts["status:err-diff"] + rep.Counts["status:exit-diff"]
	fmt.Printf("\nmatrix: cases=%d  replayable=%d  skipped=%d\n", len(cases), run, rep.Counts["status:skipped"])
	fmt.Printf("  same %d | out-diff %d | err-wording %d | err-diff %d | exit-diff %d\n",
		rep.Counts["status:same"], rep.Counts["status:out-diff"], rep.Counts["status:err-wording"],
		rep.Counts["status:err-diff"], rep.Counts["status:exit-diff"])
	if rep.Counts["status:skipped"] > 0 {
		keys := make([]string, 0, len(rep.Skips))
		for k := range rep.Skips {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  skip %-45s %d\n", k, rep.Skips[k])
		}
	}
	// The most useful failure list: we accepted something the oracle rejected.
	var silent []mcase
	for _, c := range cases {
		if c.Status == "exit-diff" && c.Oracle.Exit != 0 && c.Ours.Exit == 0 {
			silent = append(silent, c)
		}
	}
	fmt.Printf("\naccepted-but-should-fail: %d\n", len(silent))
	for i, c := range silent {
		if i >= 10 {
			break
		}
		fmt.Printf("  %-44s %s\n", filepath.Base(c.File), strings.Join(c.Argv, " "))
	}
}

func mkMR(stdout, stderr string, exit int, file string) mr {
	norm := normalizeStderr(stderr, file)
	outSum := sha256.Sum256([]byte(stdout))
	errSum := sha256.Sum256([]byte(norm))
	return mr{
		Exit: exit,
		Out:  hex.EncodeToString(outSum[:]),
		Err:  hex.EncodeToString(errSum[:]),
		Msgs: stderrMsgs(norm),
		Head: firstNonEmptyLine(norm),
		Len:  len(stdout),
	}
}

func classifyMatrix(o, s mr) string {
	switch {
	case o.Exit != s.Exit:
		return "exit-diff"
	case o.Out != s.Out:
		return "out-diff"
	case o.Err == s.Err:
		return "same"
	case equalStrings(o.Msgs, s.Msgs):
		return "err-wording"
	}
	return "err-diff"
}

func captureInput(timeout time.Duration, bin string, args []string, stdin []byte) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
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

func writeMatrix(path string, rep mreport) {
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
