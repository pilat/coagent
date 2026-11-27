package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	skillNamePattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	skillDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type listedSkillEntry struct {
	URI         string          `json:"uri"`
	Frontmatter map[string]any  `json:"frontmatter"`
	Resources   json.RawMessage `json:"resources"`
}

type skillResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   *int64 `json:"size"`
}

func decodeSkillEntry(raw json.RawMessage) (Skill, string) {
	var entry listedSkillEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Skill{}, "invalid entry: " + err.Error()
	}

	name, _ := entry.Frontmatter["name"].(string)

	skill := Skill{URI: entry.URI, Name: name, Frontmatter: entry.Frontmatter}
	if string(entry.Resources) == `"dynamic"` {
		return skill, "dynamic manifest"
	}

	if len(entry.Resources) == 0 || entry.Resources[0] != '[' {
		return skill, "invalid entry: resources is missing or malformed"
	}

	files, reason := decodeSkillResources(entry.Resources)
	if reason != "" {
		return Skill{}, reason
	}

	if reason := validateSkillHeader(entry); reason != "" {
		return skill, reason
	}

	if reason := validateSkillResources(entry.URI, files); reason != "" {
		return skill, reason
	}

	for _, file := range files {
		skill.Files = append(skill.Files, SkillFile{URI: file.URI, Digest: file.Digest, Size: *file.Size})
	}

	return skill, ""
}

func decodeSkillResources(raw json.RawMessage) ([]skillResource, string) {
	var files []skillResource

	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, "invalid entry: decode resources: " + err.Error()
	}

	return files, ""
}

func validateSkillHeader(entry listedSkillEntry) string {
	if !strings.HasSuffix(entry.URI, "/SKILL.md") {
		return "invalid entry: uri does not end in /SKILL.md"
	}

	name, _ := entry.Frontmatter["name"].(string)
	if len(name) > 64 || !skillNamePattern.MatchString(name) {
		return "invalid entry: name is missing or not a valid skill name"
	}

	root := strings.TrimSuffix(entry.URI, "/SKILL.md")
	if root[strings.LastIndex(root, "/")+1:] != name {
		return "invalid entry: name does not match the skill path"
	}

	description, _ := entry.Frontmatter["description"].(string)
	if description == "" {
		return "invalid entry: description is missing"
	}

	return ""
}

func validateSkillResources(uri string, files []skillResource) string {
	if len(files) > 512 {
		return "more than 512 files"
	}

	seen := make(map[string]bool)
	root := strings.TrimSuffix(uri, "/SKILL.md") + "/"
	var total int64

	for _, file := range files {
		if reason := validateSkillFile(file, root, seen); reason != "" {
			return "invalid entry: " + reason
		}

		if *file.Size > 16*1024*1024-total {
			return "more than 16 MiB"
		}

		total += *file.Size
		seen[file.URI] = true
	}

	if !seen[uri] {
		return "invalid entry: SKILL.md is not in resources"
	}

	return ""
}

func validateSkillFile(file skillResource, root string, seen map[string]bool) string {
	switch {
	case !strings.HasPrefix(file.URI, root):
		return fmt.Sprintf("file %s is outside the skill directory", file.URI)
	case seen[file.URI]:
		return fmt.Sprintf("file %s is listed twice", file.URI)
	case !skillDigestPattern.MatchString(file.Digest):
		return fmt.Sprintf("file %s has a malformed digest", file.URI)
	case file.Size == nil || *file.Size < 0:
		return fmt.Sprintf("file %s has no size", file.URI)
	default:
		return ""
	}
}
