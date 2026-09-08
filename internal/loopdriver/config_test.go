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
