package register

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// A host can remove a published machine in the control panel. The
// marketplace then tells the agent (POST /withdrawn), or answers its next
// capability report with 410 Gone; either way the agent stops hosting and
// says so on every start, until it is removed or registered again.

// WithdrawnMessage is what `status`, `check` and the daemon log say.
const WithdrawnMessage = "This machine was removed from the marketplace in the control panel. " +
	"Run 'sudo gpu-agent remove' to uninstall the agent, or register again with a new code to list it again."

// Withdrawal is withdrawn.json.
type Withdrawal struct {
	At int64 `json:"at"`
	// By is how the agent learned it: "control panel" (POST /withdrawn) or
	// "capability report" (410).
	By string `json:"by"`
}

// WithdrawnPath is where the record lives: the data directory, which
// `gpu-agent remove` deletes and a new registration clears.
func WithdrawnPath() string { return filepath.Join(config.DataDir(), "withdrawn.json") }

// MarkWithdrawn records that the listing was removed.
func MarkWithdrawn(by string) error {
	if err := os.MkdirAll(config.DataDir(), 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(Withdrawal{At: time.Now().Unix(), By: by}, "", "  ")
	return os.WriteFile(WithdrawnPath(), data, 0600)
}

// LoadWithdrawn is the record, or nil when the machine is listed.
func LoadWithdrawn() *Withdrawal {
	data, err := os.ReadFile(WithdrawnPath())
	if err != nil {
		return nil
	}
	var w Withdrawal
	if json.Unmarshal(data, &w) != nil {
		return &Withdrawal{By: "unknown"}
	}
	return &w
}

// ClearWithdrawn forgets it: the machine was registered again.
func ClearWithdrawn() error {
	if err := os.Remove(WithdrawnPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
