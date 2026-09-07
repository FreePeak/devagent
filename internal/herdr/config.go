// Config glue for the herdr package: config.Load with the TS loadConfig()
// default (cwd) and error pass-through.
package herdr

import "github.com/FreePeak/devagent/internal/config"

// loadConfigOrEmpty loads devagent.json from repoPath (CONFIG_FILENAMES
// order); err is the exact config-validation error callers match on.
func loadConfigOrEmpty(repoPath string) (config.Config, error) {
	return config.Load(repoPath)
}
