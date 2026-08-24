package configtools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
)

func parseWhisperPatch(raw json.RawMessage) (configops.WhisperPatch, error) {
	if isNull(raw) {
		return configops.WhisperPatch{Set: true}, nil
	}

	fields, err := readJSONObject(raw)
	if err != nil {
		return configops.WhisperPatch{}, fmt.Errorf("whisper must be an object with provider and model: %w", err)
	}

	for key := range fields {
		if key != "provider" && key != "model" {
			return configops.WhisperPatch{}, fmt.Errorf("unknown whisper field %q", key)
		}
	}

	if len(fields) != 2 {
		return configops.WhisperPatch{}, errors.New("whisper provider and model are required")
	}

	provider, providerSet, err := stringField(fields, "provider")
	if err != nil {
		return configops.WhisperPatch{}, err
	}

	model, modelSet, err := stringField(fields, "model")
	if err != nil {
		return configops.WhisperPatch{}, err
	}

	if !providerSet || !modelSet || provider == "" || model == "" {
		return configops.WhisperPatch{}, errors.New("whisper provider and model are required")
	}

	return configops.WhisperPatch{
		Set:   true,
		Value: &config.ManagerWhisperEntry{Provider: provider, Model: model},
	}, nil
}

func readJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}

	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("expected object")
	}

	fields := make(map[string]json.RawMessage)

	for decoder.More() {
		key, err := readObjectKey(decoder)
		if err != nil {
			return nil, err
		}

		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}

		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("read value: %w", err)
		}

		fields[key] = raw
	}

	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("read closing brace: %w", err)
	}

	if err := requireEOF(decoder); err != nil {
		return nil, err
	}

	return fields, nil
}

func readObjectKey(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}

	key, ok := token.(string)
	if !ok {
		return "", errors.New("expected string key")
	}

	return key, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing data after object")
		}

		return fmt.Errorf("trailing data after object: %w", err)
	}

	return nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeInt64Array(raw json.RawMessage) ([]int64, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, fmt.Errorf("unmarshal array: %w", err)
	}

	values := make([]int64, len(elements))
	for i, element := range elements {
		if isNull(element) {
			return nil, fmt.Errorf("element %d is null", i)
		}

		if err := json.Unmarshal(element, &values[i]); err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
	}

	return values, nil
}
