package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// The three reports answer three different questions and use three different
// schemas. `-summary` folds them into one table so a reviewer (or a hook) can
// see "did this change help?" without reading 800KB of JSON. It also stores the
// table in conformance/score.json so the next run can print a delta.

type scoreJSON struct {
	Generated     string         `json:"generated"`
	OracleVersion string         `json:"oracle_version"`
	Compare       map[string]int `json:"compare"`
	Matrix        map[string]int `json:"matrix"`
	Stderr        map[string]int `json:"stderr"` // status counts + spoken/msg-match/full-match
}

func runSummary(path string) {
	score := scoreJSON{
		Generated:     time.Now().UTC().Format(time.RFC3339),
		OracleVersion: "unknown",
		Compare:       map[string]int{},
		Matrix:        map[string]int{},
		Stderr:        map[string]int{},
	}

	var cmp struct {
		OracleVersion string         `json:"oracle_version"`
		Counts        map[string]int `json:"counts"`
	}
	if readJSON("conformance/status.json", &cmp) == nil {
		score.Compare = stripStatus(cmp.Counts)
		if cmp.OracleVersion != "" {
			score.OracleVersion = cmp.OracleVersion
		}
	}

	var st struct {
		Counts map[string]int `json:"counts"`
		Cases  []struct {
			Status string `json:"status"`
			Oracle struct {
				Msgs []string `json:"msgs"`
			} `json:"oracle"`
			Ours struct {
				Msgs []string `json:"msgs"`
			} `json:"ours"`
		} `json:"cases"`
	}
	if readJSON("conformance/stderr.json", &st) == nil {
		spoken, msg, full := 0, 0, 0
		for _, c := range st.Cases {
			if c.Status == "skipped" {
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
		score.Stderr = map[string]int{
			"spoken":     spoken,
			"msg-match":  msg,
			"full-match": full,
			"same":       st.Counts["status:same"],
			"diff":       st.Counts["status:diff"],
			"skipped":    st.Counts["status:skipped"],
		}
	}

	var mx struct {
		Counts map[string]int `json:"counts"`
		Skips  map[string]int `json:"skips"`
	}
	if readJSON("conformance/matrix.json", &mx) == nil {
		score.Matrix = mx.Counts
		score.Matrix["skipped"] = mx.Counts["status:skipped"]
	}

	prev := scoreJSON{}
	if readJSON(path, &prev) != nil {
		prev = scoreJSON{}
	}

	fmt.Printf("\n== score ==  oracle %s\n", score.OracleVersion)
	line := func(label string, cur, old map[string]int, order ...string) {
		fmt.Printf("%-8s", label)
		for _, k := range order {
			v := cur[k]
			if old != nil {
				if d := v - old[k]; d != 0 {
					fmt.Printf("  %s %d (%+d)", k, v, d)
					continue
				}
			}
			fmt.Printf("  %s %d", k, v)
		}
		fmt.Println()
	}
	line("compare", score.Compare, prev.Compare, "match", "diff", "reject-match", "timeout", "skipped")
	line("stderr", score.Stderr, prev.Stderr, "msg-match", "full-match", "spoken", "skipped")
	line("matrix", score.Matrix, prev.Matrix, "status:same", "status:exit-diff", "status:out-diff", "status:err-diff", "status:err-wording", "status:skipped")
	if len(prev.Compare) > 0 {
		fmt.Printf("delta vs %s\n", prev.Generated)
	}

	if b, err := json.MarshalIndent(score, "", " "); err == nil {
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
			return
		}
		fmt.Printf("wrote %s\n", path)
	}
}

// stripStatus turns {"status:match": 1} into {"match": 1} so the compare row
// reads like the other two.
func stripStatus(counts map[string]int) map[string]int {
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		if len(k) > 7 && k[:7] == "status:" {
			k = k[7:]
		}
		if k == "scope:skipped" {
			k = "skipped"
		}
		out[k] = v
	}
	return out
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// sortedKeys keeps report helpers deterministic.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
