package loader

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

const skillDescriptionMaxRunes = 1536

// sourceInfo tracks where a skill or subagent was loaded from.
// Used to apply plugin prefixes to marketplace items.
type sourceInfo struct {
	path       string // filesystem path to the skill/agent directory or file
	pluginName string // plugin name for marketplace items, empty for local
}

type Skill struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	UserInvocable          *bool  `yaml:"user-invocable"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
	Content                string `yaml:"-"`
	Path                   string `yaml:"-"`
}

type Subagent struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
	Model       string   `yaml:"model,omitempty"`
	Prompt      string   `yaml:"-"`
	Path        string   `yaml:"-"`
}

// IsUserInvocable returns true if the skill can be invoked by users.
// Defaults to true if UserInvocable is nil.
func (s *Skill) IsUserInvocable() bool {
	return s.UserInvocable == nil || *s.UserInvocable
}

// IsModelInvocable reports whether the model may discover and invoke this skill.
func (s *Skill) IsModelInvocable() bool {
	return !s.DisableModelInvocation
}

// AnnouncementDescription returns the bounded description exposed to models.
func (s *Skill) AnnouncementDescription() string {
	runes := []rune(s.Description)
	if len(runes) <= skillDescriptionMaxRunes {
		return s.Description
	}

	return string(runes[:skillDescriptionMaxRunes])
}

// parseFrontmatterFile reads a file and splits it into YAML frontmatter and content.
// Returns (nil, content, nil) when the file has no frontmatter.
func parseFrontmatterFile(path string) ([]byte, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open frontmatter file %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	return parseFrontmatter(file, path)
}

// parseFrontmatter splits a --- delimited YAML header from the body. label names
// the source in errors, so an embedded skill reports something readable rather
// than a path that does not exist on disk.
func parseFrontmatter(r io.Reader, label string) ([]byte, string, error) {
	scanner := bufio.NewScanner(r)

	var fm strings.Builder
	var body strings.Builder

	inFrontmatter := false
	frontmatterDone := false

	for scanner.Scan() {
		line := scanner.Text()

		if frontmatterDone {
			body.WriteString(line)
			body.WriteString("\n")

			continue
		}

		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true

				continue
			}

			frontmatterDone = true

			continue
		}

		if inFrontmatter {
			fm.WriteString(line)
			fm.WriteString("\n")

			continue
		}

		body.WriteString(line)
		body.WriteString("\n")
	}

	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("scan frontmatter file %s: %w", label, err)
	}

	fmBytes := []byte{}
	if fm.Len() > 0 {
		fmBytes = []byte(fm.String())
	}

	return fmBytes, strings.TrimSpace(body.String()), nil
}
