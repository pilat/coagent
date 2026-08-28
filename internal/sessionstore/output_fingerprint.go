package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// OutputFingerprint makes deterministic identities for producers that reconcile
// an existing durable fact instead of inventing a fresh standalone notice.
func OutputFingerprint(kind OutputType, content string, sessionID int64, attributes map[string]any) string {
	return outputFingerprintWithRelease(kind, content, sessionID, attributes, false)
}

func OutputFingerprintWithRelease(
	kind OutputType,
	content string,
	sessionID int64,
	attributes map[string]any,
	releasesInput bool,
) string {
	return outputFingerprintWithRelease(kind, content, sessionID, attributes, releasesInput)
}

func outputFingerprintWithRelease(
	kind OutputType,
	content string,
	sessionID int64,
	attributes map[string]any,
	releasesInput bool,
) string {
	payload := struct {
		Type          OutputType      `json:"type"`
		Content       string          `json:"content"`
		SessionID     int64           `json:"session_id"`
		Attributes    json.RawMessage `json:"attributes"`
		ReleasesInput bool            `json:"releases_input,omitempty"`
	}{
		Type: kind, Content: content, SessionID: sessionID,
		Attributes:    fingerprintAttributes(kind, attributes),
		ReleasesInput: releasesInput,
	}
	encoded, _ := json.Marshal(payload) //nolint:errchkjson // closed internal payload contains only JSON values.
	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:])
}

func fingerprintAttributes(kind OutputType, attributes map[string]any) json.RawMessage {
	type messageAttributes struct {
		Source           string          `json:"source,omitempty"`
		Waiting          json.RawMessage `json:"waiting,omitempty"`
		WaitingIdentity  json.RawMessage `json:"waiting_identity,omitempty"`
		ProgressRevision string          `json:"progress_revision,omitempty"`
	}
	type openedAttributes struct {
		Name    string `json:"name"`
		WorkDir string `json:"work_dir"`
	}
	type replacedAttributes struct {
		OldSessionID int64  `json:"old_session_id"`
		NewSessionID int64  `json:"new_session_id"`
		Name         string `json:"name"`
		WorkDir      string `json:"work_dir"`
	}
	type closedAttributes struct {
		Reason string `json:"reason"`
	}

	var value any = struct{}{}

	switch kind {
	case OutputMessageReplaceable, OutputMessagePersistent:
		waiting, err := json.Marshal(attributes["waiting"])
		if err != nil {
			return nil
		}

		identity, err := json.Marshal(attributes["waiting_identity"])
		if err != nil {
			return nil
		}

		value = messageAttributes{
			Source:           stringValue(attributes["source"]),
			Waiting:          waiting,
			WaitingIdentity:  identity,
			ProgressRevision: stringValue(attributes["progress_revision"]),
		}
	case OutputSessionOpened:
		value = openedAttributes{
			Name:    stringValue(attributes[outputAttributeName]),
			WorkDir: stringValue(attributes[outputAttributeWorkDir]),
		}
	case OutputSessionReplaced:
		oldID, _ := positiveInt64(attributes["old_session_id"])
		newID, _ := positiveInt64(attributes["new_session_id"])
		value = replacedAttributes{
			OldSessionID: oldID,
			NewSessionID: newID,
			Name:         stringValue(attributes[outputAttributeName]),
			WorkDir:      stringValue(attributes[outputAttributeWorkDir]),
		}
	case OutputSessionClosed:
		value = closedAttributes{Reason: stringValue(attributes["reason"])}
	}

	encoded, _ := json.Marshal(value) //nolint:errchkjson // closed typed attributes contain JSON primitives only.

	return encoded
}

func stringValue(value any) string {
	text, _ := value.(string)

	return text
}

func outputFingerprint(kind OutputType, content string, sessionID int64, attributes map[string]any) string {
	return OutputFingerprint(kind, content, sessionID, attributes)
}

func validOutputType(kind OutputType) bool {
	switch kind {
	case OutputMessageReplaceable,
		OutputMessagePersistent,
		OutputSessionOpened,
		OutputSessionReplaced,
		OutputSessionClosed:
		return true
	default:
		return false
	}
}

func isMessageOutput(kind OutputType) bool {
	return kind == OutputMessageReplaceable || kind == OutputMessagePersistent
}
