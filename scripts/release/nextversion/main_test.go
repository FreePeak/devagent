package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// fakeGit maps argv[0] (plus range/limit args) to canned output; any
// lookup miss returns "" — the same "error = empty" contract the real
// runGit implements.
type fakeGit map[string]string

func (f fakeGit) gitFn(args ...string) string {
	if out, ok := f[joinArgs(args)]; ok {
		return out
	}
	return ""
}

func joinArgs(args []string) string {
	key := ""
	for i, a := range args {
		if i > 0 {
			key += "\x00"
		}
		key += a
	}
	return key
}

func lsRemoteKey() string  { return joinArgs([]string{"ls-remote", "--tags", "origin", "refs/tags/v*"}) }
func localTagsKey() string { return joinArgs([]string{"tag", "--list", "v*"}) }

func TestBumpForPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		subjects []string
		want     string
	}{
		{"empty", nil, "none"},
		{"untyped only", []string{"Merge pull request #1", "Release notes"}, "none"},
		{"fix patch", []string{"chore: tidy", "fix: crash on empty repo"}, "patch"},
		{"feat minor", []string{"fix: a", "feat: b"}, "minor"},
		{"feat before fix, minor wins", []string{"feat: a", "fix: b"}, "minor"},
		{"bang major short-circuits", []string{"fix: a", "feat!: new protocol"}, "major"},
		{"bang in scope", []string{"fix(cli)!: rewrite flag parsing"}, "major"},
		{"breaking anywhere in line is major", []string{"fix: handle BREAKING CHANGE payloads"}, "major"},
		{"breaking lowercase in line", []string{"feat: stop breaking the world"}, "major"},
		{"scope stripped for type match", []string{"feat(api): add endpoint"}, "minor"},
		{"no space after colon is not typed", []string{"feat:nope"}, "none"},
		{"docs floor to patch", []string{"docs: update PRD"}, "patch"},
		{"docs after minor stays minor", []string{"feat: a", "docs: b"}, "minor"},
		{"fix after minor no-op", []string{"feat: a", "fix: b"}, "minor"},
		{"chore after fix stays patch", []string{"fix: a", "chore: b"}, "patch"},
		{"colonless", []string{"just words"}, "none"},
		{"multiline body line counted as subject", []string{"fix: subject"}, "patch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bumpFor(tc.subjects); got != tc.want {
				t.Fatalf("bumpFor(%q) = %q, want %q", tc.subjects, got, tc.want)
			}
		})
	}
}
func TestLastTagFilteringAndSort(t *testing.T) {
	raw := "" +
		"abc123\trefs/tags/v1.9.0\n" +
		"def456\trefs/tags/v1.10.0\n" +
		"aaa111\trefs/tags/v1.2\n" + // two-part: dropped
		"bbb222\trefs/tags/v1.2.3-rc1\n" + // prerelease: dropped
		"ccc333\trefs/tags/v1.2.3\n" +
		"ddd444\trefs/tags/v1.2.3^{}\n" + // peeled duplicate suffix: dropped
		"eee555\trefs/tags/notasemver\n" + // no v prefix: dropped
		"\n" + // empty line: dropped
		"fff666\trefs/tags/v0.9.9\n"
	f := fakeGit{lsRemoteKey(): raw}
	got := lastTag(f.gitFn)
	if got != "v1.10.0" {
		t.Fatalf("lastTag = %q, want v1.10.0 (numeric sort: v1.10.0 > v1.9.0)", got)
	}
}

func TestLastTagLocalFallbackWhenNoRemote(t *testing.T) {
	// Empty ls-remote (no origin / network down) must trigger the local
	// tag list, still wrapped in the refs/tags/ shape.
	f := fakeGit{
		lsRemoteKey():  "",
		localTagsKey(): "v0.1.0\nv0.2.0\n",
	}
	if got := lastTag(f.gitFn); got != "v0.2.0" {
		t.Fatalf("lastTag = %q, want v0.2.0 from local fallback", got)
	}
}

func TestLastTagNoneAnywhere(t *testing.T) {
	f := fakeGit{lsRemoteKey(): "", localTagsKey(): ""}
	g := f.gitFn
	if got := lastTag(g); got != "" {
		t.Fatalf("lastTag = %q, want empty", got)
	}
}

func TestSubjectsSinceRanges(t *testing.T) {
	logKey := func(rng string) string { return joinArgs([]string{"log", "--format=%s", rng}) }
	f := fakeGit{
		logKey("v1.2.3..HEAD"): "feat: one\n\nfix: two\n  \n",
	}
	got := subjectsSince(f.gitFn, "v1.2.3")
	want := []string{"feat: one", "fix: two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subjectsSince = %q, want %q", got, want)
	}
}

func TestSubjectsSinceMidRaceFallsBackTo20(t *testing.T) {
	// Mid-race: remote tag exists but local snapshot lacks its commit —
	// the range query fails (fakeGit miss → empty). Must fall back to
	// the bounded -20 window, NOT return zero subjects.
	rngKey := joinArgs([]string{"log", "--format=%s", "v1.2.3..HEAD"})
	winKey := joinArgs([]string{"log", "--format=%s", "-20"})
	f := fakeGit{
		rngKey: "", // git error → ""
		winKey: "fix: recent one\nfix: recent two\n",
	}
	got := subjectsSince(f.gitFn, "v1.2.3")
	want := []string{"fix: recent one", "fix: recent two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subjectsSince = %q, want %q", got, want)
	}
}

func TestSubjectsSinceNoTagQueriesHead(t *testing.T) {
	headKey := joinArgs([]string{"log", "--format=%s", "HEAD"})
	f := fakeGit{headKey: "feat: bootstrap\n"}
	got := subjectsSince(f.gitFn, "")
	want := []string{"feat: bootstrap"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subjectsSince = %q, want %q", got, want)
	}
}

func TestComputeNoTags(t *testing.T) {
	f := fakeGit{
		lsRemoteKey():  "",
		localTagsKey(): "",
		joinArgs([]string{"log", "--format=%s", "HEAD"}): "feat: initial\n",
	}
	got := compute(f.gitFn)
	if got.Prev != "0.0.0" || got.Next != "0.1.0" || got.Bump != "minor" || got.PrCount != 1 {
		t.Fatalf("compute = %+v, want prev 0.0.0 next 0.1.0 minor prCount 1", got)
	}
}

func TestComputeFromExistingTag(t *testing.T) {
	f := fakeGit{
		lsRemoteKey(): "abc\trefs/tags/v1.2.3\n",
		joinArgs([]string{"log", "--format=%s", "v1.2.3..HEAD"}): "feat: x\nfix: y\n",
	}
	got := compute(f.gitFn)
	if got.Prev != "1.2.3" || got.Next != "1.3.0" || got.Bump != "minor" || got.PrCount != 2 {
		t.Fatalf("compute = %+v, want prev 1.2.3 next 1.3.0 minor prCount 2", got)
	}
}

func TestComputePatchFloorOnUntypedOnly(t *testing.T) {
	f := fakeGit{
		lsRemoteKey(): "abc\trefs/tags/v2.5.9\n",
		joinArgs([]string{"log", "--format=%s", "v2.5.9..HEAD"}): "Merge pull request #9\nsome release note\n",
	}
	got := compute(f.gitFn)
	if got.Next != "2.5.10" || got.Bump != "none" || got.PrCount != 2 {
		t.Fatalf("compute = %+v, want next 2.5.10 bump none prCount 2 (prCount counts ALL subjects)", got)
	}
}

func TestJSONFieldOrder(t *testing.T) {
	g := fakeGit{lsRemoteKey(): "", localTagsKey(): ""}.gitFn
	got := compute(g)
	// nextFrom("0.0.0","none") → 0.0.1
	want := `{"prev":"0.0.0","next":"0.0.1","bump":"none","prCount":0}`
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("json = %s, want %s (field order prev,next,bump,prCount is a pinned contract)", b, want)
	}
}

func TestSemverComponentsQuirks(t *testing.T) {
	if c := semverComponents("v1.2.3"); c != [3]int{1, 2, 3} {
		t.Fatalf("semverComponents(v1.2.3) = %v", c)
	}
	if c := semverComponents("10.0.0"); c != [3]int{10, 0, 0} {
		t.Fatalf("semverComponents(10.0.0) = %v", c)
	}
}
