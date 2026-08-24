package configtools

import (
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/configops"
)

func applyManagerPatchFields(fields map[string]json.RawMessage, patch *configops.ManagerPatch) error {
	parsers := []struct {
		key string
		set func() error
	}{
		{"driver", func() error { return setStringField(fields, "driver", &patch.Driver) }},
		{managerFieldEnabled, func() error { return setBoolField(fields, managerFieldEnabled, &patch.Enabled) }},
		{"bot_token", func() error { return setStringField(fields, "bot_token", &patch.BotToken) }},
		{
			"allowed_user_ids",
			func() error { return setInt64SliceField(fields, "allowed_user_ids", &patch.AllowedUserIDs) },
		},
		{"target_chat_id", func() error { return setInt64Field(fields, "target_chat_id", &patch.TargetChatID) }},
		{
			"service_topic_name",
			func() error { return setStringField(fields, "service_topic_name", &patch.ServiceTopicName) },
		},
		{"service_topic_icon_emoji_id", func() error {
			return setStringField(fields, "service_topic_icon_emoji_id", &patch.ServiceTopicIconEmojiID)
		}},
		{"session_topic_icon_emoji_id", func() error {
			return setStringField(fields, "session_topic_icon_emoji_id", &patch.SessionTopicIconEmojiID)
		}},
		{
			"send_chunk_delay_ms",
			func() error { return setIntField(fields, "send_chunk_delay_ms", &patch.SendChunkDelayMS) },
		},
		{"poll_timeout_sec", func() error { return setIntField(fields, "poll_timeout_sec", &patch.PollTimeoutSec) }},
	}
	for _, parser := range parsers {
		if err := parser.set(); err != nil {
			return err
		}
	}

	return setWhisperField(fields, &patch.Whisper)
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return "", false, nil
	}

	if isNull(raw) {
		return "", true, fmt.Errorf("%s cannot be null", key)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, fmt.Errorf("%s must be a string: %w", key, err)
	}

	return value, true, nil
}

func boolField(fields map[string]json.RawMessage, key string) (bool, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return false, false, nil
	}

	if isNull(raw) {
		return false, true, fmt.Errorf("%s cannot be null", key)
	}

	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, fmt.Errorf("%s must be a boolean: %w", key, err)
	}

	return value, true, nil
}

func intField(fields map[string]json.RawMessage, key string) (int, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, false, nil
	}

	if isNull(raw) {
		return 0, true, fmt.Errorf("%s cannot be null", key)
	}

	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("%s must be an integer: %w", key, err)
	}

	return value, true, nil
}

func int64Field(fields map[string]json.RawMessage, key string) (int64, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, false, nil
	}

	if isNull(raw) {
		return 0, true, fmt.Errorf("%s cannot be null", key)
	}

	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("%s must be an integer: %w", key, err)
	}

	return value, true, nil
}

func int64SliceField(fields map[string]json.RawMessage, key string) ([]int64, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, false, nil
	}

	if isNull(raw) {
		return nil, true, fmt.Errorf("%s cannot be null", key)
	}

	value, err := decodeInt64Array(raw)
	if err != nil {
		return nil, true, fmt.Errorf("%s must be an array of integers: %w", key, err)
	}

	return value, true, nil
}

func setStringField(fields map[string]json.RawMessage, key string, dst **string) error {
	value, present, err := stringField(fields, key)
	if err != nil {
		return err
	}

	if present {
		*dst = &value
	}

	return nil
}

func setBoolField(fields map[string]json.RawMessage, key string, dst **bool) error {
	value, present, err := boolField(fields, key)
	if err != nil {
		return err
	}

	if present {
		*dst = &value
	}

	return nil
}

func setIntField(fields map[string]json.RawMessage, key string, dst **int) error {
	value, present, err := intField(fields, key)
	if err != nil {
		return err
	}

	if present {
		*dst = &value
	}

	return nil
}

func setInt64Field(fields map[string]json.RawMessage, key string, dst **int64) error {
	value, present, err := int64Field(fields, key)
	if err != nil {
		return err
	}

	if present {
		*dst = &value
	}

	return nil
}

func setInt64SliceField(fields map[string]json.RawMessage, key string, dst **[]int64) error {
	value, present, err := int64SliceField(fields, key)
	if err != nil {
		return err
	}

	if present {
		*dst = &value
	}

	return nil
}

func setWhisperField(fields map[string]json.RawMessage, dst *configops.WhisperPatch) error {
	raw, ok := fields["whisper"]
	if !ok {
		return nil
	}

	value, err := parseWhisperPatch(raw)
	if err != nil {
		return err
	}

	*dst = value

	return nil
}
