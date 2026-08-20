package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/tool"
)

const editDescription = `Edits a file using exact string matching. Always read the file first to see its contents.

- old_string must match the target text EXACTLY, including whitespace and indentation.
- By default old_string must match exactly one location; if it matches multiple, the edit is rejected — add surrounding context to make it unique.
- If old_string is not found, the edit is rejected — read the file again to get the current content.
- new_string replaces the matched text. Use an empty string to delete.
- Set replace_all: true to replace EVERY occurrence of old_string instead of requiring a unique match.

Copy the exact text from the file — whitespace matters.

Examples:
- Replace a line: old_string: "    old line content\n", new_string: "    new line content\n"
- Delete lines: old_string: "line to delete\n", new_string: ""
- Insert after a line: old_string: "existing line\n", new_string: "existing line\nnew inserted line\n"
- Rename everywhere: old_string: "oldName", new_string: "newName", replace_all: true`

var _ tool.Tool = (*editTool)(nil)

type editParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string,omitempty"`
	NewString  string `json:"new_string,omitempty"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type editTool struct {
	workDir string
	lspMgr  lsp.Manager
	mutator fileMutator
}

func newEditTool(workDir string, lspMgr lsp.Manager, mutator fileMutator) *editTool {
	return &editTool{workDir: workDir, lspMgr: lspMgr, mutator: mutator}
}

func (t *editTool) ID() string          { return "edit" }
func (t *editTool) Description() string { return editDescription }

func (t *editTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "The path to the file to modify"
			},
			"old_string": {
				"type": "string",
				"description": "The exact text to find and replace. Must match uniquely in the file."
			},
			"new_string": {
				"type": "string",
				"description": "The replacement text. Use empty string to delete the matched text."
			},
			"replace_all": {
				"type": "boolean",
				"description": "Replace every occurrence of old_string instead of requiring a unique match. Defaults to false."
			}
		},
		"required": ["file_path", "old_string", "new_string"]
	}`)
}

func (t *editTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.edit")

	p, err := t.parseEditParams(params, log)
	if err != nil {
		return nil, err
	}

	filePath := resolvePath(t.workDir, p.FilePath)

	newContent, finalRanges, hasReplaceAll, err := t.applyEdit(ctx, filePath, p)
	if err != nil {
		return nil, err
	}

	log.Info("applied", zap.String("filePath", filePath))

	output := t.buildOutput(newContent, finalRanges, hasReplaceAll)

	diagnostics, err := t.lspDiagnostics(ctx, filePath, log)
	if err != nil {
		return nil, err
	}

	output += diagnostics

	title := filePath

	if t.workDir != "" {
		if rel, err := filepath.Rel(t.workDir, filePath); err == nil {
			title = rel
		}
	}

	return &tool.Result{
		Title:  title,
		Output: output,
		Metadata: map[string]any{
			"filePath": filePath,
		},
	}, nil
}

func (t *editTool) parseEditParams(params json.RawMessage, log *zap.Logger) (editParams, error) {
	var p editParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))
		return p, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.FilePath == "" {
		return p, errors.New("file_path is required")
	}

	if p.OldString == "" {
		return p, errors.New("old_string is required")
	}

	return p, nil
}

func (t *editTool) applyEdit(
	ctx context.Context,
	filePath string,
	p editParams,
) (string, []editRange, bool, error) {
	unlock := lockFileWrite(filePath)
	defer unlock()

	if err := rejectNonRegular(filePath); err != nil {
		return "", nil, false, err
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, false, fmt.Errorf("file not found: %s", filePath)
		}

		return "", nil, false, fmt.Errorf("read file: %w", err)
	}

	var newContent string

	var finalRanges []editRange
	if p.ReplaceAll {
		newContent, finalRanges, err = executeReplaceAll(string(content), p.OldString, p.NewString)
	} else {
		newContent, finalRanges, err = executeStrReplace(string(content), p.OldString, p.NewString)
	}

	if err != nil {
		return "", nil, false, err
	}

	if err := t.mutator.WriteFile(ctx, filePath, []byte(newContent), false); err != nil {
		return "", nil, false, fmt.Errorf("write file: %w", err)
	}

	return newContent, finalRanges, p.ReplaceAll, nil
}

func (t *editTool) buildOutput(newContent string, finalRanges []editRange, hasReplaceAll bool) string {
	output := "Edit applied successfully."
	if len(finalRanges) == 0 {
		return output
	}

	finalLines := strings.Split(newContent, "\n")
	if hasReplaceAll {
		output += "\n" + tieredReplaceAllPreview(finalLines, finalRanges)
	} else {
		output += "\n" + formatContextPreview(finalLines, finalRanges)
	}

	return output
}

func (t *editTool) lspDiagnostics(ctx context.Context, filePath string, log *zap.Logger) (string, error) {
	if t.lspMgr == nil {
		return "", nil
	}

	log.Debug("edit: checking LSP diagnostics",
		zap.String("file", filePath),
		zap.String("workDir", t.workDir),
	)

	if _, err := t.lspMgr.GetDiagnostics(ctx, t.workDir, filePath); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		log.Debug("edit: LSP diagnostics unavailable", zap.Error(err))

		return "", nil
	}

	diagnostics := t.lspMgr.GetAllDiagnostics(ctx, t.workDir, 20, 5)
	log.Debug("edit: LSP diagnostics",
		zap.String("file", filePath),
		zap.Int("filesWithErrors", len(diagnostics)),
	)

	if len(diagnostics) == 0 {
		return "", nil
	}

	diagStr := lsp.FormatDiagnostics(diagnostics)
	if diagStr == "" {
		return "", nil
	}

	return "\n\n" + diagStr, nil
}
