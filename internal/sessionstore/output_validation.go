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

//nolint:gocyclo // Closed output variants validate their paired producer payloads at one boundary.
func validateProducerAttributes(kind OutputType, attributes map[string]any) error {
	allowed := map[string]struct{}{}

	switch kind {
	case OutputMessage:
		allowed["source"] = struct{}{}

		if source, ok := attributes["source"]; ok && source != outputSourceScheduler && source != outputSourceAgent {
			return errors.New("invalid message output source")
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
		allowed["cancelled_processes"] = struct{}{}

		if attributes["reason"] != killedReason {
			return errors.New("session closed output requires killed reason")
		}

		if rawCount, exists := attributes["cancelled_processes"]; exists {
			count, ok := rawCount.(float64)
			if !ok || count < 0 || count != float64(int(count)) {
				return errors.New("session closed output requires a nonnegative process count")
			}
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
