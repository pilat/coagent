package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	browserUndefined browserState = iota
	browserNull
	browserSet
)

var removedManagerKeys = map[string]bool{
	"service_topic_name":          true,
	"service_topic_icon_emoji_id": true,
	"session_topic_icon_emoji_id": true,
	"target_chat_id":              true,
}

type browserState int

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
	if err := scanTopLevel(&document, found); err != nil {
		return nil, nil, err
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

// scanTopLevel rejects a null browser section, which strict decoding would read as
// absent, and strips retired manager keys.
func scanTopLevel(document *yaml.Node, found map[string]bool) error {
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	root := document.Content[0]
	if browserStateOf(root, make(map[*yaml.Node]bool)) == browserNull {
		return errors.New("browser must be a mapping with a non-empty mcp_name")
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "managers" {
			stripManagerSequence(root.Content[i+1], found)
		}
	}

	return nil
}

// browserStateOf follows aliases and merge keys with yaml.v3 precedence: an explicit
// key wins, then the first merge source that defines it.
func browserStateOf(node *yaml.Node, visited map[*yaml.Node]bool) browserState {
	node = resolveAlias(node)
	if node == nil || visited[node] {
		return browserUndefined
	}

	visited[node] = true
	if node.Kind == yaml.SequenceNode {
		return firstDefinedBrowser(node.Content, visited)
	}

	if node.Kind != yaml.MappingNode {
		return browserUndefined
	}

	var merges []*yaml.Node

	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "browser":
			if value := resolveAlias(node.Content[i+1]); value != nil && value.Tag == "!!null" {
				return browserNull
			}

			return browserSet
		case "<<":
			merges = append(merges, node.Content[i+1])
		}
	}

	return firstDefinedBrowser(merges, visited)
}

func firstDefinedBrowser(sources []*yaml.Node, visited map[*yaml.Node]bool) browserState {
	for _, source := range sources {
		if state := browserStateOf(source, visited); state != browserUndefined {
			return state
		}
	}

	return browserUndefined
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode && depth < 64; depth++ {
		node = node.Alias
	}

	return node
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
