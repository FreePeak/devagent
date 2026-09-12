package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordReleasePersistsTagAndSHA pins the row shape at the `record
// release` CLI write site: every Q24 field the command reads from flags
// must land in the appended release-created row. PR #349 added a Revision
// field but silently dropped Tag/SHA from the ReleaseRecord literal while
// still printing "(tag @ sha)" to stdout, so rows persisted "tag":""
// "sha":"" — the exact fields the record exists to carry. No test covered
// this write path, which is why it stayed green; this closes that gap.
func TestRecordReleasePersistsTagAndSHA(t *testing.T) {
	repo := t.TempDir()

	root := NewRoot()
	root.SetArgs([]string{
		"record", "release",
		"--tag", "v1.2.3",
		"--sha", "deadbeefcafe",
		"--repo", repo,
		"--source", "cli",
	})
	root.SilenceUsage = true
	if err := root.Execute(); err != nil {
		t.Fatalf("record release: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("no release-created row written")
	}

	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("unmarshal row %q: %v", line, err)
	}

	for key, want := range map[string]string{
		"event":   "release-created",
		"tag":     "v1.2.3",
		"sha":     "deadbeefcafe",
		"version": "1.2.3",
		"source":  "cli",
	} {
		if got, _ := row[key].(string); got != want {
			t.Errorf("row[%q] = %q, want %q", key, got, want)
		}
	}
}
