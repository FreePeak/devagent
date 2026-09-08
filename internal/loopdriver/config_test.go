package loopdriver

import "testing"

func TestConfigFromEnvDevagentBin(t *testing.T) {
	repo := "/tmp/any"
	cfg := ConfigFromGetenv(repo, func(string) string { return "" })
	if cfg.DevagentBin != "" {
		t.Fatalf("default env should leave DevagentBin unset (WithDefaults applies %q): %q", "devagent", cfg.DevagentBin)
	}
	cfg = ConfigFromGetenv(repo, func(key string) string {
		if key == "SELFBUILD_DEVAGENT_BIN" {
			return "/tmp/devagent-soak"
		}
		return ""
	})
	if cfg.DevagentBin != "/tmp/devagent-soak" {
		t.Fatalf("SELFBUILD_DEVAGENT_BIN not honored: %q", cfg.DevagentBin)
	}
	d := cfg.WithDefaults()
	if d.DevagentBin != "/tmp/devagent-soak" {
		t.Fatalf("WithDefaults clobbered the override: %q", d.DevagentBin)
	}
	empty := LoopConfig{}.WithDefaults()
	if empty.DevagentBin != "devagent" {
		t.Fatalf("WithDefaults must apply the devagent fallback: %q", empty.DevagentBin)
	}
}

func TestConfigFromEnvTestCmd(t *testing.T) {
	repo := "/tmp/any"
	cfg := ConfigFromGetenv(repo, func(string) string { return "" })
	if cfg.TestCmd != "npm test" {
		t.Fatalf("default env should apply the npm-test gate: %q", cfg.TestCmd)
	}
	cfg = ConfigFromGetenv(repo, func(key string) string {
		if key == "SELFBUILD_TEST_CMD" {
			return "go test ./internal/loopdriver/..."
		}
		return ""
	})
	if cfg.TestCmd != "go test ./internal/loopdriver/..." {
		t.Fatalf("SELFBUILD_TEST_CMD not honored: %q", cfg.TestCmd)
	}
	d := cfg.WithDefaults()
	if d.TestCmd != "go test ./internal/loopdriver/..." {
		t.Fatalf("WithDefaults clobbered the override: %q", d.TestCmd)
	}
	empty := LoopConfig{}.WithDefaults()
	if empty.TestCmd != "npm test" {
		t.Fatalf("WithDefaults must apply the npm-test fallback: %q", empty.TestCmd)
	}
}
