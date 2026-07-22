package daemon

import (
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/configops"
)

// ConfigApplier commits a staged change under a marker, then restarts. It does
// not deliver the verdict: success is only knowable after the restart.
type ConfigApplier struct {
	ops     configops.Service
	restart func()

	mu      sync.Mutex
	claimed bool
}

// NewConfigApplier binds the ops layer to the daemon's restart trigger.
func NewConfigApplier(ops configops.Service, restart func()) *ConfigApplier {
	return &ConfigApplier{ops: ops, restart: restart}
}

// Ops is the mutation layer, for staging and secrets.
func (a *ConfigApplier) Ops() configops.Service { return a.ops }

// PendingCall is the config call a marker on disk still owes sessionID a result
// for, zero when it owes none. The in-memory ledger dies with the restart the
// apply itself causes, so the marker is what keeps that call owned across it.
func (a *ConfigApplier) PendingCall(sessionID int64) (configops.Pending, error) {
	p, err := a.ops.LoadPending()
	if err != nil {
		return configops.Pending{}, fmt.Errorf("load pending-apply marker: %w", err)
	}

	if p == nil || p.SessionID != sessionID {
		return configops.Pending{}, nil
	}

	return *p, nil
}

// ClaimApply reserves the single apply slot, daemon-wide. The marker, the config
// file and the restart are global, so a second commit would overwrite the first.
func (a *ConfigApplier) ClaimApply() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.claimed {
		return false
	}

	a.claimed = true

	return true
}

// ReleaseApply gives the slot back after a change that never reached disk. A
// committed one keeps it: only the restart it caused clears the slot.
func (a *ConfigApplier) ReleaseApply() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.claimed = false
}

// Apply commits and asks for the restart. A rejected commit comes back to the
// caller, which must answer immediately — no verdict is coming.
func (a *ConfigApplier) Apply(staged *configops.Staged, p configops.Pending) configops.Verdict {
	if v := a.ops.Commit(staged, p); v.Failed() {
		a.ReleaseApply()

		return v
	}

	a.Restart()

	return configops.OK()
}

// Restart brings the daemon back with no config change — what a rotated
// credential needs, since the file it is referenced from did not move.
func (a *ConfigApplier) Restart() {
	if a.restart != nil {
		a.restart()
	}
}
