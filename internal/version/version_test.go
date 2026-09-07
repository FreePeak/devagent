package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestVersionMatchesPackageJSON pins the Go version constant to package.json,
// which the Node implementation re-exports as DEVAGENT_VERSION (src/version.ts).
// Same-commit version parity is the FR-GO-01 acceptance criterion; drift here
// means one implementation was bumped without the other.
func TestVersionMatchesPackageJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if pkg.Version == "" {
		t.Fatal("package.json has empty version field")
	}
	if Version != pkg.Version {
		t.Fatalf("version drift: Go %q vs package.json %q — update both src/version.ts and internal/version/version.go together", Version, pkg.Version)
	}
}
