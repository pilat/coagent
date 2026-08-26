package sessionevent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	NotifyMessage        NotificationType = "message"
	NotifyHeartbeat      NotificationType = "heartbeat"
	NotifyStateChanged   NotificationType = "state_changed"
	NotifyInputReceived  NotificationType = "input_received"
	NotifySessionCreated NotificationType = "session_created"
	NotifySessionCleared NotificationType = "session_cleared"
	NotifySecretRequest  NotificationType = "secret_request"
	NotifySecretResolved NotificationType = "secret_resolved"
	NotifyWaiting        NotificationType = "waiting"
)

type WaitKind string

const (
	WaitSleep    WaitKind = "sleep"
	WaitSubagent WaitKind = "subagent"
)

// WaitItem is one authoritative external obligation that currently suspends a
// session. The daemon projects these from schedule/subagent ledgers; clients do
// not infer waiting from transcript text.
type WaitItem struct {
	Kind    WaitKind
	ChildID int64
	WakeAt  *time.Time
}

// State is the ephemeral runtime state published to controllers. It is not the
// persisted session status or the subagent link lifecycle.
type State string

const (
	StateRunning State = "running"
	StateIdle    State = "idle"
	StateError   State = "error"
)

func (s State) valid() bool {
	switch s {
	case StateRunning, StateIdle, StateError:
		return true
	default:
		return false
	}
}

// NotificationType identifies the kind of notification sent from a session.
type NotificationType string

// Notification is a message sent from a session to connected clients.
type Notification struct {
	Type          NotificationType
	Message       string
	Name          string         // only for NotifySessionCreated / NotifySessionCleared
	Status        State          // only for NotifyStateChanged
	Reason        string         // only for NotifyStateChanged
	Source        string         // "user", "agent", "scheduler" — only for NotifyInputReceived
	WorkDir       string         // only for NotifySessionCreated / NotifySessionCleared
	Attributes    map[string]any // only for NotifySessionCreated / NotifySessionCleared
	OldSessionID  int64          // only for NotifySessionCleared
	NewSessionID  int64          // only for NotifySessionCleared
	AfterOutputID int64          // only for NotifyStateChanged; 0 has no output barrier

	// RequestID correlates a NotifySecretRequest with the value that answers it,
	// and with the NotifySecretResolved that closes it everywhere else. The
	// credential itself never travels through a notification.
	RequestID string
	// SecretName is the variable a secret-request notification is about.
	SecretName string
	Waiting    []WaitItem
}

// Validate checks the discriminated-union contract carried by Notification.
// Producers still use a value struct for cheap fan-out, so this is the boundary
// that prevents missing required fields and payload from a different event type
// from reaching controllers.
func (n Notification) Validate() error {
	allowed, err := n.variantContract()
	if err != nil {
		return err
	}

	return n.rejectUnexpectedFields(allowed)
}

func (n Notification) variantContract() (map[string]bool, error) {
	switch n.Type {
	case NotifyMessage:
		return fields("message"), n.require(n.Message != "", "message")
	case NotifyHeartbeat:
		return fields(), nil
	case NotifyStateChanged:
		if err := n.require(n.Status.valid(), "status running, idle, or error"); err != nil {
			return nil, err
		}

		if err := n.require(n.AfterOutputID >= 0, "non-negative output barrier"); err != nil {
			return nil, err
		}

		return fields("status", "reason", "after_output_id"), nil
	case NotifyInputReceived:
		return fields("message", "source"), n.validateInput()
	case NotifySessionCreated:
		return fields("name", "work_dir", "attributes"), n.require(n.WorkDir != "", "work_dir")
	case NotifySessionCleared:
		return fields("name", "work_dir", "attributes", "old_session_id", "new_session_id"), n.validateClear()
	case NotifySecretRequest:
		return fields("message", "request_id", "secret_name"), n.validateSecretRequest()
	case NotifySecretResolved:
		return fields("request_id", "secret_name"), n.validateSecretRequest()
	case NotifyWaiting:
		return fields("message", "waiting"), n.validateWaiting()
	default:
		return nil, fmt.Errorf("unknown notification type %q", n.Type)
	}
}

func (n Notification) validateWaiting() error {
	if len(n.Waiting) == 0 {
		return n.require(false, "at least one wait item")
	}

	for _, item := range n.Waiting {
		switch item.Kind {
		case WaitSleep:
			if item.WakeAt == nil || item.ChildID != 0 {
				return errors.New("waiting sleep item requires wake_at only")
			}
		case WaitSubagent:
			if item.ChildID <= 0 || item.WakeAt != nil {
				return errors.New("waiting subagent item requires positive child_id only")
			}
		default:
			return fmt.Errorf("unknown wait kind %q", item.Kind)
		}
	}

	return nil
}

// FormatWaiting gives text-only clients an honest projection of the structured
// wait set. Multiple foreground subagents are an all-wait set.
func FormatWaiting(items []WaitItem) string {
	lines := make([]string, 0, len(items)+2)
	allSleep := len(items) > 0

	if len(items) > 1 {
		lines = append(lines, "⏳ Waiting for all:")
	}

	for _, item := range items {
		switch item.Kind {
		case WaitSleep:
			prefix := "⏳ Sleeping until "
			if len(items) > 1 {
				prefix = "• sleep until "
			}

			lines = append(lines, prefix+item.WakeAt.Local().Format("15:04 02 Jan"))
		case WaitSubagent:
			allSleep = false

			prefix := "⏳ Waiting for subagent"
			if len(items) > 1 {
				prefix = "• subagent"
			}

			lines = append(lines, fmt.Sprintf("%s #%d", prefix, item.ChildID))
		}
	}

	if allSleep {
		lines = append(lines, "💬 Send a message to interrupt sleep")
	} else {
		lines = append(lines, "💬 New messages will be queued until this wait completes")
	}

	return strings.Join(lines, "\n")
}

func (n Notification) validateInput() error {
	if err := n.require(n.Message != "", "message"); err != nil {
		return err
	}

	return n.require(validInputSource(n.Source), "source user, agent, or scheduler")
}

func (n Notification) validateClear() error {
	checks := []struct {
		ok    bool
		field string
	}{
		{n.WorkDir != "", "work_dir"},
		{n.OldSessionID > 0, "positive old_session_id"},
		{n.NewSessionID > 0, "positive new_session_id"},
		{n.OldSessionID != n.NewSessionID, "distinct session IDs"},
	}

	for _, check := range checks {
		if err := n.require(check.ok, check.field); err != nil {
			return err
		}
	}

	return nil
}

func (n Notification) validateSecretRequest() error {
	if err := n.require(n.RequestID != "", "request_id"); err != nil {
		return err
	}

	return n.require(n.SecretName != "", "secret_name")
}

func (n Notification) require(ok bool, field string) error {
	if !ok {
		return fmt.Errorf("%s notification requires %s", n.Type, field)
	}

	return nil
}

func fields(names ...string) map[string]bool {
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}

	return result
}

func (n Notification) rejectUnexpectedFields(allowed map[string]bool) error {
	for field, present := range n.presentFields() {
		if present && !allowed[field] {
			return fmt.Errorf("%s notification carries unexpected %s", n.Type, field)
		}
	}

	return nil
}

func validInputSource(source string) bool {
	switch source {
	case "user", "agent", "scheduler":
		return true
	default:
		return false
	}
}

func (n Notification) presentFields() map[string]bool {
	return map[string]bool{
		"message":        n.Message != "",
		"name":           n.Name != "",
		"status":         n.Status != "",
		"reason":         n.Reason != "",
		"source":         n.Source != "",
		"work_dir":       n.WorkDir != "",
		"attributes":     n.Attributes != nil,
		"old_session_id": n.OldSessionID != 0,
		"new_session_id": n.NewSessionID != 0,
		"request_id":     n.RequestID != "",
		"secret_name":    n.SecretName != "",
		"waiting":        len(n.Waiting) > 0,
	}
}
