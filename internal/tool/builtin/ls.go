package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const lsDescription = `Lists the contents of a directory.

Usage:
- The path parameter can be absolute or relative to the working directory
- Returns file names with sizes and directory indicators
- Directories are shown first, followed by files
- Hidden files (starting with .) are excluded
- Files show their size in human-readable format (B, KB, MB, GB)`

type LsParams struct {
	Path string `json:"path"`
}

type LsTool struct {
	workDir string
}

var _ tool.Tool = (*LsTool)(nil)

type entryInfo struct {
	name  string
	isDir bool
	size  int64
}

func NewLsTool(workDir string) *LsTool {
	return &LsTool{workDir: workDir}
}

func (t *LsTool) ID() string          { return "ls" }
func (t *LsTool) Description() string { return lsDescription }

func (t *LsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path to the directory to list"
			}
		},
		"required": ["path"]
	}`)
}

func (t *LsTool) Execute(_ context.Context, params json.RawMessage) (*tool.Result, error) {
	var p LsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	path := p.Path
	if path == "" {
		path = "."
	}

	path = resolvePath(t.workDir, path)

	infos, err := readDirEntries(path)
	if err != nil {
		return nil, err
	}

	output := buildLsOutput(infos)

	title := path

	if t.workDir != "" {
		if rel, err := filepath.Rel(t.workDir, path); err == nil {
			title = rel
		}
	}

	return &tool.Result{
		Title:  title,
		Output: strings.TrimSuffix(output, "\n"),
		Metadata: map[string]any{
			metaKeyPath:  path,
			metaKeyCount: len(infos),
		},
	}, nil
}

func readDirEntries(path string) ([]entryInfo, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("path not found: %s", path)
	}

	if err != nil {
		return nil, fmt.Errorf("stat path: %w", err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}

	infos := make([]entryInfo, 0, len(entries))

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		var size int64
		if fi, err := entry.Info(); err == nil {
			size = fi.Size()
		}

		infos = append(infos, entryInfo{
			name:  entry.Name(),
			isDir: entry.IsDir(),
			size:  size,
		})
	}

	sort.Slice(infos, func(i, j int) bool {
		if infos[i].isDir != infos[j].isDir {
			return infos[i].isDir
		}

		return infos[i].name < infos[j].name
	})

	return infos, nil
}

func buildLsOutput(infos []entryInfo) string {
	if len(infos) == 0 {
		return "(empty directory)\n"
	}
	var output strings.Builder

	for _, info := range infos {
		if info.isDir {
			fmt.Fprintf(&output, "%s/\n", info.name)
		} else {
			fmt.Fprintf(&output, "%s (%s)\n", info.name, formatSize(info.size))
		}
	}

	return output.String()
}

// formatSize formats a file size in human-readable form.
func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fGB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
