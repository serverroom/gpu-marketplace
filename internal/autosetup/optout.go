package autosetup

import (
	"os"
	"path/filepath"
	"strings"
)

// A host (or support) can turn the automatic setup off: a file named
// auto-setup in the agent's config directory (/etc/gpu-agent) that says "off".
// `gpu-agent setup --off` and `--on` write it.

// OptOutPath is that file.
func OptOutPath(configDir string) string { return filepath.Join(configDir, "auto-setup") }

// Enabled reports whether the automatic setup may run. On unless the file
// says off.
func Enabled(configDir string) bool {
	data, err := os.ReadFile(OptOutPath(configDir))
	if err != nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(string(data)), "off")
}

// SetEnabled turns the automatic setup on or off.
func SetEnabled(configDir string, on bool) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}
	word := "off\n"
	if on {
		word = "on\n"
	}
	return os.WriteFile(OptOutPath(configDir), []byte(word), 0644)
}
