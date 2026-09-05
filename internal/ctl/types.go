package ctl

import (
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

// ManagerControl is the introspection `status` needs from the managers runtime.
// The start error is per manager: one shared error would misreport its neighbours.
type ManagerControl interface {
	RunningIDs() []string
	StartError(id string) error
}

type BuiltinManagerControl interface {
	ID() string
	Alive() bool
}

// Deps is what the built-in ops answer from. Config may carry a nil
// UnifiedConfig — that is the legal pre-onboarding state, not an error.
type Deps struct {
	Config     *config.Config
	ConfigPath string
	Managers   ManagerControl
	Delivery   controllerapi.OutputStatusFactory
	Builtin    BuiltinManagerControl
}

// StatusResult answers `status`: daemon state, and nothing that has to be
// probed. Provider validity is proven by use, not by a check here.
type StatusResult struct {
	BinaryVersion   string `json:"binary_version"`
	ProtocolVersion int    `json:"protocol_version"`
	// BootID identifies this run of the daemon: a restart reuses the pid, socket and
	// binary, so nothing else here separates the new image from the old.
	BootID        string           `json:"boot_id"`
	PID           int              `json:"pid"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	ConfigPath    string           `json:"config_path"`
	ConfigPresent bool             `json:"config_present"`
	Providers     []ProviderStatus `json:"providers,omitempty"`
	ModelCount    int              `json:"model_count"`
	DefaultModel  string           `json:"default_model,omitempty"`
	// Search renders the integrated-search state: "tavily", "searxng
	// (<base_url>)", "native (openrouter)", "disabled", or empty when
	// unconfigured. Additive; no protocol-version bump.
	Search   string          `json:"search,omitempty"`
	Managers []ManagerStatus `json:"managers,omitempty"`
}

// SetProviderParams is the bootstrap's provider form.
//
// APIKey is the one place a credential value crosses this socket, and it travels
// exactly once: the daemon writes it into the secrets file and puts only a
// ${VAR} reference into config.yaml. It is never echoed back by any op.
type SetProviderParams struct {
	Name    string `json:"name"`
	Driver  string `json:"driver"`
	APIKey  string `json:"api_key,omitempty"`
	SAFile  string `json:"sa_file,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Catalog string `json:"catalog,omitempty"`
	// Models are model ids to enable alongside the provider. A provider with no
	// usable model is a config that cannot serve a session, so they land in the
	// same write.
	Models []string `json:"models,omitempty"`
}

// RestartResult acknowledges restart_daemon. The restart begins after this is on
// the wire, so "accepted" is the whole answer — the proof is the reconnect.
type RestartResult struct {
	Restarting bool `json:"restarting"`
}

// SetSecretParams carries one credential inbound. RequestID correlates it with
// the secret_request push that asked for it.
type SetSecretParams struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	RequestID string `json:"request_id,omitempty"`
}

// ProviderStatus is one configured provider. No credential, not even a hint:
// nothing on this socket needs to see one.
type ProviderStatus struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// ManagerStatus reports one configured manager, including why it is not running
// — the field that tells a chat "your bot token is wrong" without a probe.
type ManagerStatus struct {
	ID                string `json:"id"`
	Driver            string `json:"driver"`
	Enabled           bool   `json:"enabled"`
	Running           bool   `json:"running"`
	Error             string `json:"error,omitempty"`
	PendingOutputs    int    `json:"pending_outputs,omitempty"`
	BlockedOutputID   int64  `json:"blocked_output_id,omitempty"`
	BlockedForSeconds int64  `json:"blocked_for_seconds,omitempty"`
	DeliveryError     string `json:"delivery_error,omitempty"`
}
