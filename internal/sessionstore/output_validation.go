package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func validateOutputDraft(draft OutputDraft) error {
	if draft.SessionID <= 0 || !validOutputType(draft.Type) {
		return errors.New("invalid output draft")
	}

	if isMessageOutput(draft.Type) != (draft.Content != "") {
		return errors.New("invalid output content")
	}

	if (draft.SourceKey == "") != (draft.Fingerprint == "") {
		return errors.New("output identity requires source key and fingerprint together")
	}

	if draft.SourceKey != "" &&
		(strings.TrimSpace(draft.SourceKey) == "" || strings.TrimSpace(draft.Fingerprint) == "") {
		return errors.New("empty output identity")
	}

	if err := validateProducerAttributes(draft.Type, draft.Attributes); err != nil {
		return err
	}

	if draft.SourceKey != "" && draft.Fingerprint != outputFingerprintWithRelease(
		draft.Type,
		draft.Content,
		draft.SessionID,
		draft.Attributes,
		draft.ReleasesInput,
	) {
		return ErrOutputConflict
	}

	return nil
}

//nolint:gocyclo,wsl_v5 // Closed output variants validate their paired producer payloads at one boundary.
func validateProducerAttributes(kind OutputType, attributes map[string]any) error {
	allowed := map[string]struct{}{}

	switch kind {
	case OutputMessageReplaceable, OutputMessagePersistent:
		allowed["waiting"] = struct{}{}
		allowed["waiting_identity"] = struct{}{}
		allowed["source"] = struct{}{}
		allowed["progress_revision"] = struct{}{}

		if source, ok := attributes["source"]; ok && source != outputSourceScheduler && source != outputSourceAgent {
			return errors.New("invalid message output source")
		}

		_, waiting := attributes["waiting"]

		_, identity := attributes["waiting_identity"]
		if waiting != identity {
			return errors.New("waiting output requires matching identity")
		}
		if waiting {
			return validateWaitingPayload(attributes["waiting"], attributes["waiting_identity"])
		}
	case OutputSessionOpened:
		allowed[outputAttributeName] = struct{}{}
		allowed[outputAttributeWorkDir] = struct{}{}

		if !nonEmptyString(attributes[outputAttributeName]) || !nonEmptyString(attributes[outputAttributeWorkDir]) {
			return errors.New("session opened output requires name and work dir")
		}
	case OutputSessionReplaced:
		allowed["old_session_id"] = struct{}{}
		allowed["new_session_id"] = struct{}{}
		allowed[outputAttributeName] = struct{}{}
		allowed[outputAttributeWorkDir] = struct{}{}

		if !positiveNumber(attributes["old_session_id"]) || !positiveNumber(attributes["new_session_id"]) ||
			!nonEmptyString(attributes[outputAttributeName]) || !nonEmptyString(attributes[outputAttributeWorkDir]) {
			return errors.New("session replaced output requires roots, name, and work dir")
		}
	case OutputSessionClosed:
		allowed["reason"] = struct{}{}

		if attributes["reason"] != killedReason {
			return errors.New("session closed output requires killed reason")
		}
	}

	for key := range attributes {
		if key == managerIDAttribute || key == "message_ids" || key == ModelInputGenerationAttribute {
			return fmt.Errorf("output attributes may not set %q", key)
		}

		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown %s output attribute %q", kind, key)
		}
	}

	return nil
}

//nolint:wsl_v5 // Each paired item is validated as one discriminated waiting record.
func validateWaitingPayload(displayValue, identityValue any) error {
	display, err := waitingItems(displayValue)
	if err != nil {
		return fmt.Errorf("invalid waiting display: %w", err)
	}
	identity, err := waitingItems(identityValue)
	if err != nil {
		return fmt.Errorf("invalid waiting identity: %w", err)
	}
	if len(display) != len(identity) || len(display) == 0 {
		return errors.New("waiting output requires paired items")
	}
	for i := range display {
		if wakeAt, sleep := display[i]["wake_at"].(string); sleep {
			if wakeAt == "" || len(display[i]) != 1 || len(identity[i]) != 1 ||
				stringValue(identity[i]["tool_call_id"]) == "" {
				return errors.New("invalid waiting sleep item")
			}
			continue
		}
		childID, child := positiveInt64(display[i]["child_id"])
		identityChild, identityOK := positiveInt64(identity[i]["child_id"])
		_, activationOK := positiveInt64(identity[i]["activation_seq"])
		if !child || childID != identityChild || !identityOK || !activationOK || len(display[i]) != 1 ||
			len(identity[i]) != 2 {
			return errors.New("invalid waiting subagent item")
		}
	}

	return nil
}

//nolint:wsl_v5 // The conversion is deliberately adjacent to the schema validation it feeds.
func waitingItems(value any) ([]map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode waiting payload: %w", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil, fmt.Errorf("decode waiting payload: %w", err)
	}

	return items, nil
}

func nonEmptyString(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

func positiveNumber(value any) bool {
	switch number := value.(type) {
	case int64:
		return number > 0
	case int:
		return number > 0
	case float64:
		return number > 0 && number == float64(int64(number))
	default:
		return false
	}
}

func validateLifecycleTarget(ctx context.Context, tx *sql.Tx, draft OutputDraft, owner string) error {
	if draft.Type != OutputSessionReplaced {
		return nil
	}

	oldID, ok := positiveInt64(draft.Attributes["old_session_id"])
	if !ok {
		return errors.New("session replacement requires old root")
	}

	newID, ok := positiveInt64(draft.Attributes["new_session_id"])
	if !ok || newID != draft.SessionID || oldID == newID {
		return errors.New("session replacement must belong to its distinct new root")
	}

	var oldProject, newProject, oldParent int64

	var oldAttributes string
	if err := tx.QueryRowContext(ctx, `SELECT project_id, parent_id, attributes FROM sessions WHERE id = ?`, oldID).
		Scan(&oldProject, &oldParent, &oldAttributes); err != nil {
		return fmt.Errorf("load replaced root: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM sessions WHERE id = ?`, newID).
		Scan(&newProject); err != nil {
		return fmt.Errorf("load replacement root: %w", err)
	}

	if oldParent != 0 || oldProject != newProject {
		return errors.New("session replacement roots must share a project")
	}

	var attributes map[string]any
	if err := json.Unmarshal([]byte(oldAttributes), &attributes); err != nil {
		return fmt.Errorf("decode replaced root attributes: %w", err)
	}

	if oldOwner, _ := attributes[managerIDAttribute].(string); oldOwner != owner {
		return errors.New("session replacement roots must share a manager owner")
	}

	return nil
}

func positiveInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, number > 0
	case int:
		return int64(number), number > 0
	case float64:
		return int64(number), number > 0 && number == float64(int64(number))
	default:
		return 0, false
	}
}
