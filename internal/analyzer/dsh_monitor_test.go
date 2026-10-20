package analyzer

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestHasMonitorTargetsDSH(t *testing.T) {
	enabled := &config.Config{Clients: map[string]config.Client{
		"dsh": {Enabled: true},
	}}
	if !HasMonitorTargets(enabled) {
		t.Fatalf("enabled dsh must count as a monitor target")
	}

	disabled := &config.Config{Clients: map[string]config.Client{
		"dsh": {Enabled: false},
	}}
	if HasMonitorTargets(disabled) {
		t.Fatalf("disabled dsh must not count as a monitor target")
	}
}
