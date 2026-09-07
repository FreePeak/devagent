package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordAndTrustRoundTrip(t *testing.T) {
	repo := t.TempDir()
	if IsAgentsMdTrusted(repo) {
		t.Fatal("fresh repo must be untrusted")
	}
	p, err := RecordAgentsMd(repo)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(repo, File) {
		t.Fatalf("path = %q", p)
	}
	if !IsAgentsMdTrusted(repo) {
		t.Fatal("trust record not honored")
	}
	// JSON shape: 2-space indent + trailing newline (parity with the TS write).
	raw, _ := os.ReadFile(p)
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("trust file missing trailing newline")
	}
}

func TestRecordPreservesExistingKeys(t *testing.T) {
	repo := t.TempDir()
	p := filepath.Join(repo, File)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("{\"other\": \"keep\"}\n"), 0o644)
	if _, err := RecordAgentsMd(repo); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"other": "keep"`) {
		t.Fatalf("existing keys not preserved: %s", raw)
	}
}

func TestMalformedTrustRecordIsUntrusted(t *testing.T) {
	repo := t.TempDir()
	p := filepath.Join(repo, File)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("{broken"), 0o644)
	if IsAgentsMdTrusted(repo) {
		t.Fatal("malformed record must read as untrusted")
	}
}

func TestLoadAgentsMdModes(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".devagent"), 0o755)
	os.WriteFile(filepath.Join(repo, AgentsMdFile), []byte("  # repo context\n"), 0o644)

	if got := LoadAgentsMd(repo, ModeOff); got != "" {
		t.Fatalf("off must never read: %q", got)
	}
	if got := LoadAgentsMd(repo, ModeAsk); got != "" {
		t.Fatalf("ask without confirm must be empty: %q", got)
	}
	if got := LoadAgentsMd(repo, ModeOn); got != "# repo context" {
		t.Fatalf("on must inject trimmed: %q", got)
	}
	if _, err := RecordAgentsMd(repo); err != nil {
		t.Fatal(err)
	}
	if got := LoadAgentsMd(repo, ModeAsk); got != "# repo context" {
		t.Fatalf("ask after confirm must inject: %q", got)
	}
}
