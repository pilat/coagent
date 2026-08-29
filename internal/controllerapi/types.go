package controllerapi

import (
	"time"

	"github.com/pilat/coagent/internal/sessionevent"
)

type OutputBindingData struct {
	Driver     string         `json:"driver"`
	Attributes map[string]any `json:"attributes"`
}

// Durable output type vocabulary. controllerapi must not import sessionstore,
// so the wire values are mirrored here; a sync test in internal/daemon pins
// both spellings together.
const (
	OutputMessageReplaceable = "message_replaceable"
	OutputMessagePersistent  = "message_persistent"
	OutputSessionOpened      = "session_opened"
	OutputSessionReplaced    = "session_replaced"
	OutputSessionClosed      = "session_closed"
)

type OutputClaimData struct {
	ID                        int64          `json:"id"`
	SessionID                 int64          `json:"session_id"`
	Type                      string         `json:"type"`
	Content                   string         `json:"content"`
	Attributes                map[string]any `json:"attributes"`
	AttemptID                 string         `json:"attempt_id"`
	AttemptSeq                int64          `json:"attempt_seq"`
	SourceKey                 string         `json:"source_key,omitempty"`
	SessionAttributes         map[string]any `json:"session_attributes"`
	PreviousMessageAttributes map[string]any `json:"previous_message_attributes,omitempty"`
	PreviousMessageType       string         `json:"previous_message_type,omitempty"`
	ReleasesInput             bool           `json:"releases_input"`
	// ModelInputGeneration snapshots this row's insertion-time generation.
	// Presence-bearing: nil is legacy absence, while zero is a valid value.
	ModelInputGeneration *int64 `json:"model_input_generation,omitempty"`
	// PreviousModelInputGeneration snapshots the preceding delivered message
	// output's generation with the same nil-means-legacy rule.
	PreviousModelInputGeneration *int64 `json:"previous_model_input_generation,omitempty"`
}

type ProgressData struct {
	SessionID       int64     `json:"session_id"`
	Revision        string    `json:"revision"`
	OutboxWatermark int64     `json:"outbox_watermark"`
	ObservedAt      time.Time `json:"observed_at"`
	Rendered        string    `json:"rendered"`
}

type OutputAckData struct {
	ID           int64          `json:"id"`
	AttemptID    string         `json:"attempt_id"`
	MessageIDs   []string       `json:"message_ids"`
	SessionPatch map[string]any `json:"session_patch,omitempty"`
}

type OutputRetryData struct {
	ID        int64     `json:"id"`
	AttemptID string    `json:"attempt_id"`
	Error     string    `json:"error"`
	NextAt    time.Time `json:"next_at"`
}

type OutputBlockData struct {
	ID        int64  `json:"id"`
	AttemptID string `json:"attempt_id"`
	Error     string `json:"error"`
}

type OutputQueueStatusData struct {
	Pending       int    `json:"pending"`
	BlockedID     int64  `json:"blocked_id,omitempty"`
	BlockedForSec int64  `json:"blocked_for_seconds,omitempty"`
	DeliveryError string `json:"delivery_error,omitempty"`
}

// Runtime session states (not persisted, derived from in-memory map).
const (
	StateRunning = sessionevent.StateRunning
	StateIdle    = sessionevent.StateIdle
	StateError   = sessionevent.StateError
)

// CoagentSystemProjectName is the durable logical identity of the local
// configuration project. Its directory is separate because ':' is reserved
// from user project names.
const (
	CoagentSystemProjectName = "sys:coagent"
	CoagentSystemProjectDir  = "sys_coagent"
	// BuiltinCLIManagerID is the reserved owner of the local configuration chat.
	BuiltinCLIManagerID = "cli"
	// SessionAttributeManagerID is the durable owner of a root session. Manager
	// subscriptions are routed by this value, so one manager never receives
	// another manager's conversation.
	SessionAttributeManagerID = "manager_id"
)

// State aliases the notification-layer runtime state for controller clients.
// The alias keeps one vocabulary owner while preserving controllerapi's public
// surface.
type State = sessionevent.State

// SessionCreateData defines input for creating a session.
type SessionCreateData struct {
	WorkDir       string         `json:"work_dir"`
	Prompt        string         `json:"prompt"`
	Model         string         `json:"model,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	UseWorktree   bool           `json:"use_worktree,omitempty"`
	SystemProject string         `json:"system_project,omitempty"`
}

// SessionMessageData defines one durable normal message for a session.
type SessionMessageData struct {
	SessionID int64  `json:"session_id"`
	Message   string `json:"message"`
}

// SessionKillData defines input for killing a session.
type SessionKillData struct {
	SessionID int64 `json:"session_id"`
}

// SessionStopData defines input for stopping a session's active loop without
// killing the session — the conversation is preserved and the session can be
// resumed by sending a new message.
type SessionStopData struct {
	SessionID int64 `json:"session_id"`
}

// SessionClearData defines input for clearing a session.
type SessionClearData struct {
	SessionID int64 `json:"session_id"`
}

// SessionSetModelData defines input for changing a session model.
type SessionSetModelData struct {
	SessionID      int64  `json:"session_id"`
	Model          string `json:"model"`
	ReasoningLevel string `json:"reasoning_level,omitempty"` // defaults to "medium"
}

// SessionSetAttributesData defines input for setting session attributes.
type SessionSetAttributesData struct {
	SessionID  int64          `json:"session_id"`
	Attributes map[string]any `json:"attributes"`
}

// ConfigSkillsData defines input for listing skills.
type ConfigSkillsData struct {
	SessionID int64 `json:"session_id,omitempty"`
}

// FsListDirData defines input for listing directories.
type FsListDirData struct {
	Path string `json:"path,omitempty"`
}

// ProjectCreateData defines input for provisioning (get-or-create) a
// daemon-managed folder-project by name under the configured projects root.
type ProjectCreateData struct {
	Name   string `json:"name"`
	System bool   `json:"system,omitempty"`
}

// ProjectCreateResultData is the payload for a created-or-opened project.
type ProjectCreateResultData struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// ProjectListResultData is the payload for a recency-sorted listing of
// daemon-managed folder-projects (the /new picker).
type ProjectListResultData struct {
	Projects []RecentProjectInfo `json:"projects"`
}

// RecentProjectInfo describes one daemon-managed project in a recency listing.
// LastActivity is nil when the project has no sessions yet (sorts as newest).
type RecentProjectInfo struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
}

type SessionInfo struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	WorkDir        string         `json:"work_dir"`
	ProjectID      int64          `json:"project_id,omitempty"`
	HasActiveLoop  bool           `json:"has_active_loop"`
	Model          string         `json:"model,omitempty"`
	ReasoningLevel string         `json:"reasoning_level,omitempty"`
	Status         string         `json:"status,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
	KilledAt       *time.Time     `json:"killed_at,omitempty"`
}

type ConfigModelsResultData struct {
	Models    []ConfigModelInfo `json:"models"`
	DefaultID string            `json:"default_id,omitempty"`
}

// ConfigModelInfo describes an available model. Everything but ID comes from the
// provider's catalog; prices are USD per 1M tokens.
type ConfigModelInfo struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`

	// EffortLevels are the reasoning levels this model accepts, weakest first.
	// Empty means the model exposes no effort choice — offer no effort step.
	EffortLevels []string `json:"effort_levels,omitempty"`
	// DefaultEffort is the level a switch to this model lands on; empty when
	// EffortLevels is.
	DefaultEffort string `json:"default_effort,omitempty"`
}

type ConfigSkillsResultData struct {
	Skills []ConfigSkillInfo `json:"skills"`
}

type ConfigSkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ScheduleListData defines input for listing a session's schedules.
type ScheduleListData struct {
	SessionID int64 `json:"session_id"`
}

type ScheduleListResultData struct {
	Schedules []ScheduleInfo `json:"schedules"`
}

type ScheduleInfo struct {
	ID          int64      `json:"id"`
	Cron        string     `json:"cron,omitempty"`        // 5-field expr (TZ stripped); empty for one-shot
	Timezone    string     `json:"timezone,omitempty"`    // cron timezone, e.g. "Europe/Berlin"
	OneShotAt   *time.Time `json:"one_shot_at,omitempty"` // set for one-shot (sleep) schedules
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	Fresh       bool       `json:"fresh"`
	Prompt      string     `json:"prompt,omitempty"`
}

type FsDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type FsListDirResultData struct {
	Dirs      []FsDirEntry `json:"dirs"`
	Favorites []string     `json:"favorites"`
	Home      string       `json:"home"`
	Path      string       `json:"path,omitempty"`   // actual resolved path
	Parent    string       `json:"parent,omitempty"` // parent directory (for ".." navigation)
}
