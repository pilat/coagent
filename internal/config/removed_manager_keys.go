package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

var removedManagerKeys = map[string]bool{
	"service_topic_name":          true,
	"assistant_topic_name":        true,
	"service_topic_icon_emoji_id": true,
	"session_topic_icon_emoji_id": true,
}

func stripRemovedManagerKeys(data []byte) ([]byte, []string, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node

	if err := decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parsing config file: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, nil, fmt.Errorf("parsing config file: %w", err)
		}

		return nil, nil, errors.New("parsing config file: multiple YAML documents are not supported")
	}

	found := make(map[string]bool)

	if len(document.Content) > 0 {
		root := document.Content[0]
		if root.Kind == yaml.MappingNode {
			for i := 0; i < len(root.Content); i += 2 {
				if root.Content[i].Value == "managers" {
					stripManagerSequence(root.Content[i+1], found)
				}
			}
		}
	}

	if len(found) == 0 {
		return data, nil, nil
	}

	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	filtered, err := yaml.Marshal(&document)
	if err != nil {
		return nil, nil, fmt.Errorf("remove retired manager keys: %w", err)
	}

	return filtered, keys, nil
}

func stripManagerSequence(node *yaml.Node, found map[string]bool) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}

	if node == nil || node.Kind != yaml.SequenceNode {
		return
	}

	visited := make(map[*yaml.Node]bool)
	for _, manager := range node.Content {
		stripManagerMapping(manager, found, visited)
	}
}

func stripManagerMapping(node *yaml.Node, found map[string]bool, visited map[*yaml.Node]bool) {
	if node == nil || visited[node] {
		return
	}

	visited[node] = true
	if node.Kind == yaml.AliasNode {
		stripManagerMapping(node.Alias, found, visited)
		return
	}

	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			stripManagerMapping(child, found, visited)
		}

		return
	}

	if node.Kind != yaml.MappingNode {
		return
	}

	kept := node.Content[:0]
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if removedManagerKeys[key.Value] {
			found[key.Value] = true
			continue
		}

		if key.Value == "<<" {
			stripManagerMapping(value, found, visited)
		}

		kept = append(kept, key, value)
	}

	node.Content = kept
}
