// Command nextversion computes the next semantic version from
// Conventional-Commit subjects since the last v* tag. The release
// workflow runs it via `go run ./scripts/release/nextversion`
// (issues #249, #250: Go-only release tooling).
//
// Covers both merge commits and squash-merged PRs (squash titles are
// exactly the PR title). Major > minor > patch: the highest bump
// present wins:
//
//   - feat:               -> minor
//   - fix:                -> patch
//   - ! / BREAKING CHANGE -> major
//   - anything else (docs:, chore:, config:, refactor:) -> patch floor
//
// Output is one compact JSON line, e.g.
// {"prev":"1.2.3","next":"1.4.0","bump":"minor","prCount":42}.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// gitFn mirrors the original script's git() helper: on any failure —
// including the mid-race "unknown revision" from `git log vX.Y.Z..HEAD`
// when the local snapshot lacks the remote tag's commit — it returns
// empty output instead of an error, so callers treat failure as
// "no data" and the bounded -20 fallback still fires.
type gitFn func(args ...string) string

// runGit is the production gitFn.
func runGit(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

var semverTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// lastTag returns the newest v* semver tag ("" when none). The tag must
// come from the REMOTE, not the local checkout snapshot: two pushes
// landing close together race — run A tags vX.Y.Z after run B checked
// out, so B's local list misses the new tag and computes the same
// version again (gh release create then fails HTTP 422; live 2026-09-01:
// v0.9.3/v0.9.4 double-tap). ls-remote sees the authoritative tag list
// regardless of checkout timing. With no origin (scratch repos, vendored
// use) it falls back to the local tag list — still better than describe
// because the sort is numeric and non-semver tags drop.
func lastTag(git gitFn) string {
	var lines []string
	if raw := git("ls-remote", "--tags", "origin", "refs/tags/v*"); strings.TrimSpace(raw) != "" {
		lines = strings.Split(raw, "\n")
	} else {
		for _, t := range strings.Split(git("tag", "--list", "v*"), "\n") {
			lines = append(lines, "refs/tags/"+strings.TrimSpace(t))
		}
	}

	tags := make([]string, 0, len(lines))
	for _, line := range lines {
		name := ""
		if _, rest, ok := strings.Cut(line, "refs/tags/"); ok {
			name = strings.TrimSpace(rest)
		}
		if !semverTagRe.MatchString(name) {
			continue // v1.2, v1.2.3-rc1, peeled ^{} duplicates, junk
		}
		tags = append(tags, name)
	}
	sort.Slice(tags, func(i, j int) bool {
		return semverLess(semverComponents(tags[i]), semverComponents(tags[j]))
	})
	if len(tags) == 0 {
		return ""
	}
	return tags[len(tags)-1]
}

// subjectsSince returns the commit subjects the bump scan considers.
// When the local snapshot lacks the remote tag's commit (mid-race
// checkout), the tag range comes back empty or errors — fall back to a
// bounded window of recent subjects so the bump floor stays correct.
func subjectsSince(git gitFn, last string) []string {
	var raw string
	if last == "" {
		raw = git("log", "--format=%s", "HEAD")
	} else {
		raw = git("log", "--format=%s", last+"..HEAD")
	}
	subs := nonEmptyLines(raw)
	if last != "" && len(subs) == 0 {
		subs = nonEmptyLines(git("log", "--format=%s", "-20"))
	}
	return subs
}

func nonEmptyLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var (
	typeRe     = regexp.MustCompile(`^(\w+)(\([^)]*\))?(!)?:\s`)
	breakingRe = regexp.MustCompile(`(?i)breaking`)
)

// bumpFor scans subjects in order; the highest bump wins. Note that
// breakingRe tests the WHOLE subject line: a fix that merely mentions
// breaking changes in its title is a major bump — quirk preserved from
// the original script.
func bumpFor(subs []string) string {
	bump := "none"
	for _, s := range subs {
		m := typeRe.FindStringSubmatch(s)
		if m == nil {
			continue // untyped lines (e.g. "Merge pull request ...") never affect the bump
		}
		if m[3] != "" || breakingRe.MatchString(s) {
			return "major"
		}
		switch {
		case m[1] == "feat" && bump != "major":
			bump = "minor"
		case (m[1] == "fix" || bump == "none") && bump != "major" && bump != "minor":
			bump = "patch"
		}
	}
	return bump
}

// nextFrom applies the bump to prev; bare "none" (docs/chore/config/
// refactor-only, or no typed commits at all) floors to a patch bump.
func nextFrom(prev, bump string) string {
	c := semverComponents(prev)
	switch bump {
	case "major":
		return fmt.Sprintf("%d.0.0", c[0]+1)
	case "minor":
		return fmt.Sprintf("%d.%d.0", c[0], c[1]+1)
	default:
		return fmt.Sprintf("%d.%d.%d", c[0], c[1], c[2]+1)
	}
}

// semverComponents parses MAJOR.MINOR.PATCH digits (an optional leading
// v is allowed for tags); unparsable parts count as 0, matching
// Number() in the original script.
func semverComponents(ver string) [3]int {
	var c [3]int
	for i, p := range strings.SplitN(strings.TrimPrefix(ver, "v"), ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		c[i] = n
	}
	return c
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// result is marshaled in field order: the release workflow consumes the
// exact compact JSON line via jq (.next), and the byte shape is pinned
// by tests.
type result struct {
	Prev    string `json:"prev"`
	Next    string `json:"next"`
	Bump    string `json:"bump"`
	PrCount int    `json:"prCount"`
}

func compute(git gitFn) result {
	last := lastTag(git)
	prev := "0.0.0"
	if last != "" {
		prev = strings.TrimPrefix(last, "v")
	}
	subs := subjectsSince(git, last)
	bump := bumpFor(subs)
	return result{
		Prev:    prev,
		Next:    nextFrom(prev, bump),
		Bump:    bump,
		PrCount: len(subs),
	}
}

func main() {
	out, err := json.Marshal(compute(runGit))
	if err != nil {
		fmt.Fprintln(os.Stderr, "nextversion: marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
