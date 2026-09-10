// Package eval is the FR-VAL-05 quality-drift ratchet (issue #293): it scores
// what the loop SHIPS, not whether the loop ran. The rubric is a checked-in
// file; the judge is the configured worker model dispatched through the same
// prompt/worker-adapter plumbing the audit gate's auditor uses; the verdict
// lands as an `eval-score` row on the existing orchestration ledger (no new
// event system).
//
// The ratchet half — "is this worse than the best the same goal class recently
// achieved, and which criterion regressed?" — lives in internal/ledger
// (ReadEvalScores / ClusterQualityDrift) next to the other ledger analytics, so
// `devagent ledger --clusters` (which the self-build driver already captures
// into its research prompts) reports drift for free.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RubricPath is the tracked rubric of record: it ships with the repo so an
// `eval-score` row can always be re-read against the criteria that produced it.
const RubricPath = "docs/eval/rubric.md"

// RubricOverridePath is the per-repo shadow (the path issue #293 names), read
// first when present. It stays untracked like the rest of .devagent/ run state.
const RubricOverridePath = ".devagent/eval/rubric.md"

// Criterion is one rubric line: a stable id, its weight, and what the judge
// should look for.
type Criterion struct {
	ID          string `json:"id"`
	Weight      int    `json:"weight"`
	Description string `json:"description"`
}

// Rubric is a parsed rubric file. Text is the verbatim file body handed to the
// judge, so the scored artifact and the recorded rubric cannot drift apart.
// Digest fingerprints what is actually measured (criterion ids + weights), so an
// edit that changes the measurement — one that forgets to bump `version:`, or a
// typo that silently drops a criterion line — can never share a drift baseline
// with the scores made before it. Prose-only edits leave the digest alone on
// purpose.
type Rubric struct {
	Version  string
	Digest   string
	Criteria []Criterion
	Text     string
}

// Total is the maximum attainable score (the sum of the weights).
func (r Rubric) Total() int {
	total := 0
	for _, c := range r.Criteria {
		total += c.Weight
	}
	return total
}

var (
	rubricVersionRe   = regexp.MustCompile(`^version:\s*(\S.*?)\s*$`)
	rubricCriterionRe = regexp.MustCompile(`^-\s*\[(\d+)\]\s*([^:]+):\s*(.*)$`)
)

// LoadRubric parses the rubric. path "" resolves the per-repo override at
// RubricOverridePath first, then the tracked RubricPath. A rubric with no
// version, no criteria, a duplicate id or a non-positive weight is an error
// rather than a silent zero: each of those mistakes would quietly redefine the
// baseline the ratchet guards.
func LoadRubric(repoPath, path string) (Rubric, error) {
	if path == "" {
		override := filepath.Join(repoPath, RubricOverridePath)
		if _, err := os.Stat(override); err == nil {
			path = override
		} else {
			path = filepath.Join(repoPath, RubricPath)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Rubric{}, fmt.Errorf("rubric unreadable (%s): %w", path, err)
	}
	return ParseRubric(string(data), path)
}

// ParseRubric is the pure half of LoadRubric (srcName only names errors).
func ParseRubric(src, srcName string) (Rubric, error) {
	r := Rubric{Text: src}
	seen := map[string]bool{}
	var fingerprint strings.Builder
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if m := rubricVersionRe.FindStringSubmatch(line); m != nil {
			if r.Version != "" {
				return Rubric{}, fmt.Errorf("rubric %s: duplicate version: line", srcName)
			}
			r.Version = m[1]
			continue
		}
		m := rubricCriterionRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := strings.TrimSpace(m[2])
		weight, err := strconv.Atoi(m[1])
		if err != nil || weight <= 0 {
			return Rubric{}, fmt.Errorf("rubric %s: criterion %q has a non-positive weight", srcName, id)
		}
		if seen[id] {
			return Rubric{}, fmt.Errorf("rubric %s: duplicate criterion id %q", srcName, id)
		}
		seen[id] = true
		r.Criteria = append(r.Criteria, Criterion{
			ID:          id,
			Weight:      weight,
			Description: strings.TrimSpace(m[3]),
		})
		fmt.Fprintf(&fingerprint, "%s=%d\n", id, weight)
	}
	if r.Version == "" {
		return Rubric{}, fmt.Errorf("rubric %s: no `version: <n>` line — every score must record the rubric it was measured on", srcName)
	}
	if len(r.Criteria) == 0 {
		return Rubric{}, fmt.Errorf("rubric %s: no criteria (expected `- [<weight>] <id>: <what to score>` lines)", srcName)
	}
	sum := sha256.Sum256([]byte(fingerprint.String()))
	r.Digest = hex.EncodeToString(sum[:])[:12]
	return r, nil
}
