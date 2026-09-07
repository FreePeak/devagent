package gates

import (
	"strings"
	"testing"
)

// diffFor builds a synthetic unified diff around the given added/removed
// lines. No network, no LLM: string fixtures only.
func diffFor(file string, added, removed []string) string {
	lines := []string{
		"diff --git a/" + file + " b/" + file,
		"--- a/" + file,
		"+++ b/" + file,
		"@@ -1,1 +1,1 @@",
	}
	for _, l := range removed {
		lines = append(lines, "-"+l)
	}
	for _, l := range added {
		lines = append(lines, "+"+l)
	}
	return strings.Join(lines, "\n")
}

const credLineAlt = "const api_key = " + `"sk-live-` + `src-9999";`

// TestEvaluateStridePositives ports the TS per-category positive fixtures.
func TestEvaluateStridePositives(t *testing.T) {
	cases := []struct {
		file     string
		added    []string
		removed  []string
		category string
	}{
		{"src/spoof.ts", []string{credLine}, nil, StrideCatSpoofing},
		{"src/tamper.ts", []string{"db.query(`SELECT * FROM u WHERE id = ${req.params.id}`)"}, nil, StrideCatTampering},
		{"src/repudiate.ts", []string{"// audit log call commented out"}, nil, StrideCatRepudiation},
		{"src/disclose.ts", []string{"console.log(req.body)"}, nil, StrideCatInformationDisclose},
		{"src/dos.ts", []string{"setInterval(tick, 1000)"}, nil, StrideCatDenialOfService},
		{"src/eop.ts", nil, []string{`if (user.role !== "admin") return;`}, StrideCatElevationOfPriv},
	}
	for _, tc := range cases {
		r := EvaluateStride(StrideInput{Diff: diffFor(tc.file, tc.added, tc.removed)})
		found := false
		for _, f := range r.Findings {
			if f.Category == tc.category {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected a %s finding, got %+v", tc.file, tc.category, r.Findings)
		}
	}
}

// TestEvaluateStrideNegatives ports the TS per-category benign fixtures.
func TestEvaluateStrideNegatives(t *testing.T) {
	cases := []struct {
		file     string
		added    []string
		category string
	}{
		{"src/spoof.ts", []string{"const config = loadConfig(env);"}, StrideCatSpoofing},
		{"src/tamper.ts", []string{`db.query("SELECT * FROM u WHERE id = ?", [id])`}, StrideCatTampering},
		{"src/repudiate.ts", []string{`audit("user.updated", ctx);`}, StrideCatRepudiation},
		{"src/disclose.ts", []string{`logger.info("handled request");`}, StrideCatInformationDisclose},
		{"src/dos.ts", []string{"await tick();"}, StrideCatDenialOfService},
		{"src/eop.ts", []string{`requireRole("admin");`}, StrideCatElevationOfPriv},
	}
	for _, tc := range cases {
		r := EvaluateStride(StrideInput{Diff: diffFor(tc.file, tc.added, nil)})
		for _, f := range r.Findings {
			if f.Category == tc.category {
				t.Errorf("%s: unexpected %s finding: %+v", tc.file, tc.category, f)
			}
		}
	}
}

func TestEvaluateStridePromotesCredentialToCritical(t *testing.T) {
	r := EvaluateStride(StrideInput{Diff: diffFor("src/cred.ts", []string{credLine}, nil)})
	if len(r.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	if r.Findings[0].Severity != StrideGateSeverityCritical {
		t.Errorf("severity = %q, want CRITICAL", r.Findings[0].Severity)
	}
	if r.SeverityMax != StrideGateSeverityCritical {
		t.Errorf("severityMax = %q, want CRITICAL", r.SeverityMax)
	}
}

func TestEvaluateStrideBlocksHighWithoutPromotion(t *testing.T) {
	r := EvaluateStride(StrideInput{Diff: diffFor("src/key.ts", []string{"const api_key = process.env.API_KEY;"}, nil)})
	if len(r.Findings) == 0 || r.Findings[0].Severity != StrideGateSeverityHigh {
		t.Fatalf("expected HIGH finding, got %+v", r.Findings)
	}
	if r.SeverityMax != StrideGateSeverityHigh {
		t.Errorf("severityMax = %q, want HIGH (not CRITICAL)", r.SeverityMax)
	}
}

func TestEvaluateStrideMediumIsAdvisory(t *testing.T) {
	r := EvaluateStride(StrideInput{Diff: diffFor("src/adv.ts", []string{`console.log("debug:", password);`}, nil)})
	if r.SeverityMax != StrideGateSeverityMedium {
		t.Errorf("severityMax = %q, want MEDIUM", r.SeverityMax)
	}
	for _, f := range r.Findings {
		if f.Severity == StrideGateSeverityHigh || f.Severity == StrideGateSeverityCritical {
			t.Errorf("unexpected blocking severity %q", f.Severity)
		}
	}
}

func TestEvaluateStridePassesEmptyDiff(t *testing.T) {
	for _, d := range []string{"", "   "} {
		r := EvaluateStride(StrideInput{Diff: d})
		if len(r.Findings) != 0 || r.SeverityMax != "" {
			t.Errorf("diff %q: got findings=%v severityMax=%q", d, r.Findings, r.SeverityMax)
		}
	}
}

func TestEvaluateStridePassesThroughContextDigest(t *testing.T) {
	digest := "sha256:deadbeef"
	r := EvaluateStride(StrideInput{Diff: diffFor("src/x.ts", []string{"plain line"}, nil), ContextDigest: digest, ContextDigestSet: true})
	if r.ContextDigest != digest || !r.HasDigest {
		t.Errorf("contextDigest = %q (has=%v)", r.ContextDigest, r.HasDigest)
	}
	r = EvaluateStride(StrideInput{Diff: diffFor("src/x.ts", []string{"plain line"}, nil)})
	if r.HasDigest {
		t.Errorf("digest should be absent when not set")
	}
}

func TestEvaluateStrideIncludesProvenance(t *testing.T) {
	r := EvaluateStride(StrideInput{Diff: diffFor("src/i.ts", []string{"console.log(req.body)"}, nil)})
	if len(r.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	if r.Findings[0].File != "src/i.ts" || r.Findings[0].Line == nil || *r.Findings[0].Line != 1 {
		t.Errorf("file/line = %q/%v", r.Findings[0].File, r.Findings[0].Line)
	}
}

func TestEvaluateStrideAllowlistSuppressesMatchedPaths(t *testing.T) {
	diff := diffFor("test/fixtures/credentials.json", []string{credLine}, nil)
	blocked := EvaluateStride(StrideInput{Diff: diff})
	if blocked.SeverityMax != StrideGateSeverityCritical {
		t.Fatalf("without allowlist: severityMax = %q, want CRITICAL", blocked.SeverityMax)
	}
	allowed := EvaluateStride(StrideInput{Diff: diff, AllowlistPaths: []string{"test/fixtures/**"}})
	if len(allowed.Findings) != 0 || allowed.SeverityMax != "" {
		t.Errorf("with allowlist: findings=%v severityMax=%q", allowed.Findings, allowed.SeverityMax)
	}
}

func TestEvaluateStrideAllowlistKeepsUnmatchedPaths(t *testing.T) {
	diff := diffFor("test/fixtures/credentials.json", []string{credLine}, nil) +
		"\n" + diffFor("src/cred.ts", []string{credLineAlt}, nil)
	r := EvaluateStride(StrideInput{Diff: diff, AllowlistPaths: []string{"test/fixtures/**"}})
	var files []string
	for _, f := range r.Findings {
		files = append(files, f.File)
	}
	if len(files) != 1 || files[0] != "src/cred.ts" {
		t.Errorf("files = %v, want [src/cred.ts]", files)
	}
	if r.SeverityMax != StrideGateSeverityCritical {
		t.Errorf("severityMax = %q, want CRITICAL", r.SeverityMax)
	}
}

func TestEvaluateStrideMissingAllowlistSuppressesNothing(t *testing.T) {
	diff := diffFor("src/key.ts", []string{credLine}, nil)
	r := EvaluateStride(StrideInput{Diff: diff})
	if r.SeverityMax != StrideGateSeverityCritical {
		t.Errorf("nil allowlist: severityMax = %q", r.SeverityMax)
	}
	r = EvaluateStride(StrideInput{Diff: diff, AllowlistPaths: []string{}})
	if r.SeverityMax != StrideGateSeverityCritical {
		t.Errorf("empty allowlist: severityMax = %q", r.SeverityMax)
	}
}

func TestEvaluateStrideMalformedAllowlistFailsClosed(t *testing.T) {
	for _, text := range []string{"not json", `["src/**"]`, `{"paths": "src/**"}`, `{"paths": [1]}`, "null"} {
		if _, ok := ParseStrideAllowlist(text); ok {
			t.Errorf("ParseStrideAllowlist(%q) should fail", text)
		}
	}
	parsed, ok := ParseStrideAllowlist(`{"paths": [1]}`)
	if ok {
		t.Fatalf("expected parse failure")
	}
	diff := diffFor("src/key.ts", []string{credLine}, nil)
	r := EvaluateStride(StrideInput{Diff: diff, AllowlistPaths: parsed})
	if r.SeverityMax != StrideGateSeverityCritical {
		t.Errorf("fail-closed: severityMax = %q", r.SeverityMax)
	}
}

func TestParseStrideAllowlistWellFormed(t *testing.T) {
	// Committed fixture from testdata: {"paths": ["test/**", "fixtures/*.json"]}
	text := mustRead(t, "stride-allowlist.json")
	paths, ok := ParseStrideAllowlist(text)
	if !ok {
		t.Fatal("expected the committed allowlist fixture to parse")
	}
	want := []string{"test/**", "fixtures/*.json"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
	if _, ok := ParseStrideAllowlist(""); ok {
		t.Error("empty text should not parse")
	}
}

func TestPathMatchesAllowlistGlobs(t *testing.T) {
	patterns := []string{"test/**", "src/fixtures/*.json", "golden.pem"}
	cases := []struct {
		path string
		want bool
	}{
		{"test/fixtures/creds.json", true},
		{"src/fixtures/creds.json", true},
		{"src/fixtures/nested/creds.json", false},
		{"golden.pem", true},
		{"certs/golden.pem", true},
		{"src/creds.json", false},
	}
	for _, tc := range cases {
		if got := PathMatchesAllowlist(tc.path, patterns); got != tc.want {
			t.Errorf("PathMatchesAllowlist(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestEvaluateStrideRecordedDiffFixtures pins gate pass/fail decisions on the
// committed recorded fixture diffs in testdata/diffs (acceptance: "gate
// pass/fail/skip decisions identical on recorded fixture diffs").
func TestEvaluateStrideRecordedDiffFixtures(t *testing.T) {
	credDiff := strings.Replace(mustRead(t, "diffs/high-credential.diff"), "__CREDENTIAL_LINE__", credLine, 1)
	cases := []struct {
		name       string
		diff       string
		wantPass   bool // blocked when severityMax HIGH/CRITICAL
		wantSevMax string
	}{
		{"high-credential", credDiff, false, StrideGateSeverityCritical},
		{"blocked-apikey", mustRead(t, "diffs/blocked-apikey.diff"), false, StrideGateSeverityHigh},
		{"advisory-medium", mustRead(t, "diffs/advisory-medium.diff"), true, StrideGateSeverityMedium},
		{"clean", mustRead(t, "diffs/clean.diff"), true, ""},
	}
	for _, tc := range cases {
		r := EvaluateStride(StrideInput{Diff: tc.diff})
		blocked := r.SeverityMax == StrideGateSeverityHigh || r.SeverityMax == StrideGateSeverityCritical
		if blocked == tc.wantPass {
			t.Errorf("%s: blocked=%v wantPass=%v (severityMax=%q)", tc.name, blocked, tc.wantPass, r.SeverityMax)
		}
		if r.SeverityMax != tc.wantSevMax {
			t.Errorf("%s: severityMax = %q, want %q", tc.name, r.SeverityMax, tc.wantSevMax)
		}
	}
}
