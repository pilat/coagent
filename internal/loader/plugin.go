package loader

import (
	"encoding/json"
	"fmt"
)

type pluginManifest struct {
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Author      pluginAuthor `json:"author"`
	Keywords    []string     `json:"keywords"`
	Agents      []string     `json:"agents"`
}

type pluginAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func parsePluginManifest(content string) (*pluginManifest, error) {
	var manifest pluginManifest
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		return nil, fmt.Errorf("parsing plugin manifest: %w", err)
	}

	return &manifest, nil
}
