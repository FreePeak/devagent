package herdr

import "github.com/FreePeak/devagent/internal/config"

// loadConfigOrEmpty loads devagent.json from repoPath (CONFIG_FILENAMES
// order); err is the exact config-validation error callers match on.
func loadConfigOrEmpty(repoPath string) (config.Config, error) {
	return config.Load(repoPath)
}
