package mcp

import (
	"fmt"
)

type invalidSkillEntry struct {
	reason        string
	change        func(map[string]any)
	decodeFailure bool
}

func invalidSkillEntries() []invalidSkillEntry {
	return []invalidSkillEntry{
		{reason: "dynamic manifest", change: func(e map[string]any) { e["resources"] = "dynamic" }},
		{
			reason: "invalid entry: uri does not end in /SKILL.md",
			change: func(e map[string]any) { e["uri"] = "skill://guide/no.md" },
		},
		{
			reason: "invalid entry: name is missing or not a valid skill name",
			change: func(e map[string]any) { e["frontmatter"].(map[string]any)["name"] = "Bad" },
		},
		{
			reason: "invalid entry: name does not match the skill path",
			change: func(e map[string]any) { e["uri"] = "skill://other/SKILL.md" },
		},
		{
			reason: "invalid entry: description is missing",
			change: func(e map[string]any) { delete(e["frontmatter"].(map[string]any), "description") },
		},
		{
			reason: "invalid entry: resources is missing or malformed",
			change: func(e map[string]any) { delete(e, "resources") },
		},
		{
			reason: "invalid entry: resources is missing or malformed",
			change: func(e map[string]any) { e["resources"] = "other" },
		},
		{
			reason: "invalid entry: SKILL.md is not in resources",
			change: func(e map[string]any) { e["resources"] = []any{} },
		},
		{
			reason: "invalid entry: file skill://outside/x is outside the skill directory",
			change: func(e map[string]any) { e["resources"].([]any)[0].(map[string]any)["uri"] = "skill://outside/x" },
		},
		{
			reason: "invalid entry: file skill://guide/SKILL.md is listed twice",
			change: func(e map[string]any) { e["resources"] = append(e["resources"].([]any), e["resources"].([]any)[0]) },
		},
		{
			reason: "invalid entry: file skill://guide/SKILL.md has a malformed digest",
			change: func(e map[string]any) { e["resources"].([]any)[0].(map[string]any)["digest"] = "sha256:ABC" },
		},
		{
			reason: "invalid entry: file skill://guide/SKILL.md has no size",
			change: func(e map[string]any) { delete(e["resources"].([]any)[0].(map[string]any), "size") },
		},
		{
			reason: "invalid entry: file skill://guide/SKILL.md has no size",
			change: func(e map[string]any) { e["resources"].([]any)[0].(map[string]any)["size"] = -1 },
		},
		{
			reason:        "invalid entry: decode resources: ",
			change:        func(e map[string]any) { e["resources"].([]any)[0].(map[string]any)["size"] = "12" },
			decodeFailure: true,
		},
		{
			reason: "more than 16 MiB",
			change: func(e map[string]any) { e["resources"].([]any)[0].(map[string]any)["size"] = 16*1024*1024 + 1 },
		},
		{reason: "more than 512 files", change: func(e map[string]any) {
			files := e["resources"].([]any)
			for i := range 512 {
				file := map[string]any{
					"uri":    fmt.Sprintf("skill://guide/%d", i),
					"size":   0,
					"digest": files[0].(map[string]any)["digest"],
				}
				files = append(files, file)
			}
			e["resources"] = files
		}},
	}
}
