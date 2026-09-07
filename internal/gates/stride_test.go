package gates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hunk(file string, added, removed []string, line *int) parsedDiffHunk {
	join := func(lines []string) string {
		if len(lines) == 0 {
			return ""
		}
		return strings.Join(lines, "\n") + "\n"
	}
	h := parsedDiffHunk{File: file, Added: join(added), Removed: join(removed), Line: line}
	return h
}

func linePtr(n int) *int { return &n }

// Synthetic credential fixture, assembled at runtime so this file never
// contains a usable credential-shaped literal (the gate under test still
// sees the exact same string).
const credLine = "const api_key = " + `"sk-live-` + `abcd1234";`

// mustRead reads a testdata fixture relative to this package.
func mustRead(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatalf("read testdata %s: %v", rel, err)
	}
	return string(data)
}

func TestRunStrideGateBlocksHardcodedAPIKey(t *testing.T) {
	r := RunStrideGate([]parsedDiffHunk{hunk("src/handler.ts", []string{credLine}, nil, linePtr(10))}, "/tmp/worktree")
	if r.Passed {
		t.Fatalf("expected gate to block, got %+v", r)
	}
	if r.SeverityMax != StrideSeverityHigh {
		t.Errorf("severityMax = %q, want high", r.SeverityMax)
	}
	if len(r.Findings) == 0 || r.Findings[0].Category != "S" {
		t.Errorf("findings[0].category = %v, want S", r.Findings)
	}
	if r.Gate != "G5-stride" {
		t.Errorf("gate = %q, want G5-stride", r.Gate)
	}
}

func TestRunStrideGatePassesMediumAdvisory(t *testing.T) {
	r := RunStrideGate([]parsedDiffHunk{hunk("src/handler.ts", []string{`console.log("debug:", password);`}, nil, linePtr(20))}, "/tmp/worktree")
	if !r.Passed {
		t.Fatalf("expected advisory pass, got %+v", r)
	}
	if r.SeverityMax != StrideSeverityMedium {
		t.Errorf("severityMax = %q, want medium", r.SeverityMax)
	}
	if len(r.Findings) < 1 {
		t.Errorf("expected at least one finding, got %+v", r.Findings)
	}
	if !strings.Contains(r.Detail, "STRIDE") || !strings.Contains(r.Detail, "## STRIDE G5 findings") {
		t.Errorf("detail missing STRIDE block: %q", r.Detail)
	}
}

func TestRunStrideGatePassesOnEmptyInput(t *testing.T) {
	for _, parsed := range [][]parsedDiffHunk{nil, {}} {
		r := RunStrideGate(parsed, "/tmp/wt")
		if !r.Passed {
			t.Errorf("expected pass on %v", parsed)
		}
		if len(r.Findings) != 0 {
			t.Errorf("expected zero findings on %v, got %+v", parsed, r.Findings)
		}
		if r.SeverityMax != "" {
			t.Errorf("expected empty severityMax on %v, got %q", parsed, r.SeverityMax)
		}
	}
}

func TestRunStrideGateOneFindingPerStrideLetter(t *testing.T) {
	hunks := []parsedDiffHunk{
		hunk("src/s.ts", []string{credLine}, nil, linePtr(1)),
		hunk("src/t.ts", []string{"db.query(`SELECT * FROM u WHERE id = ${req.params.id}`)"}, nil, linePtr(2)),
		hunk("src/r.ts", []string{"// audit log call removed"}, nil, linePtr(3)),
		hunk("src/i.ts", []string{"console.log(req.body)"}, nil, linePtr(4)),
		hunk("src/d.ts", []string{"setInterval(tick, 1000)"}, nil, linePtr(5)),
		hunk("src/e.ts", nil, []string{`if (user.role !== "admin") return;`}, linePtr(6)),
	}
	r := RunStrideGate(hunks, "/tmp/worktree")
	seen := map[string]bool{}
	for _, f := range r.Findings {
		seen[f.Category] = true
	}
	for _, letter := range []string{"S", "T", "R", "I", "D", "E"} {
		if !seen[letter] {
			t.Errorf("missing category %s in %+v", letter, r.Findings)
		}
	}
}

func TestRunStrideGateDuplicateLinesDistinctIDs(t *testing.T) {
	line := `console.log("debug:", password);`
	r := RunStrideGate([]parsedDiffHunk{hunk("src/dup.ts", []string{line, line}, nil, linePtr(7))}, "/tmp/worktree")
	if len(r.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(r.Findings))
	}
	if r.Findings[0].ID == r.Findings[1].ID {
		t.Errorf("duplicate ids: %q", r.Findings[0].ID)
	}
}

func TestParseUnifiedDiffSplitsHunks(t *testing.T) {
	// Recorded fixture diff with the credential line substituted at runtime.
	diff := mustRead(t, "diffs/blocked-apikey.diff")
	diff = strings.Replace(diff, "const api_key = process.env.API_KEY;", credLine, 1)
	hunks := ParseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	if hunks[0].File != "src/key.ts" {
		t.Errorf("file = %q", hunks[0].File)
	}
	if hunks[0].Line == nil || *hunks[0].Line != 1 {
		t.Errorf("line = %v, want 1", hunks[0].Line)
	}
	if hunks[0].Added != credLine+"\n" {
		t.Errorf("added = %q", hunks[0].Added)
	}
	if hunks[0].Removed != "const old = 1;\n" {
		t.Errorf("removed = %q", hunks[0].Removed)
	}
}
