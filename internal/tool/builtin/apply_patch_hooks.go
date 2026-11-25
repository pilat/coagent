package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

var (
	_ tool.HookNamed     = (*applyPatchTool)(nil)
	_ tool.HookProjector = (*applyPatchTool)(nil)
)

type editHookInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *applyPatchTool) HookName() tool.HookName {
	return tool.HookName{Aliases: []string{"patch"}}
}

func (t *applyPatchTool) HookCalls(_ context.Context, params json.RawMessage) ([]tool.HookCall, error) {
	var p applyPatchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Patch == "" {
		return nil, errors.New("patch is required")
	}

	files, err := parsePatch(p.Patch)
	if err != nil {
		return nil, fmt.Errorf("parse patch: %w", err)
	}

	if len(files) == 0 {
		return nil, errors.New("no files found in patch")
	}

	var calls []tool.HookCall

	for _, file := range files {
		projected, err := t.fileHookCalls(file)
		if err != nil {
			return nil, err
		}

		calls = append(calls, projected...)
	}

	return calls, nil
}

func (t *applyPatchTool) fileHookCalls(file patchFile) ([]tool.HookCall, error) {
	path, err := resolveAccessTarget(t.access, t.workDir, file.Path)
	if err != nil {
		return nil, err
	}

	_, err = readAccessFile(t.access, path)
	if errors.Is(err, os.ErrNotExist) {
		return t.newFileHookCall(path, file.Hunks)
	}

	if err != nil {
		return nil, fmt.Errorf("read patch file: %w", err)
	}

	calls := make([]tool.HookCall, len(file.Hunks))

	for i, hunk := range file.Hunks {
		call, err := editHunkHookCall(path, hunk)
		if err != nil {
			return nil, err
		}

		calls[i] = call
	}

	return calls, nil
}

func (t *applyPatchTool) newFileHookCall(path string, hunks []patchHunk) ([]tool.HookCall, error) {
	plan, err := planFilePatches(t.access, path, hunks)
	if err != nil {
		return nil, fmt.Errorf("plan patch file: %w", err)
	}

	input, err := json.Marshal(writeParams{
		FilePath: path,
		Content:  string(plan.content),
	})
	if err != nil {
		return nil, fmt.Errorf("encode Write hook input: %w", err)
	}

	return []tool.HookCall{{
		ToolID: "write",
		Name:   writeHookName,
		Input:  input,
	}}, nil
}

func editHunkHookCall(path string, hunk patchHunk) (tool.HookCall, error) {
	var oldLines, newLines []string

	for _, line := range hunk.Lines {
		switch line.Type {
		case ' ':
			oldLines = append(oldLines, line.Content)
			newLines = append(newLines, line.Content)
		case '-':
			oldLines = append(oldLines, line.Content)
		case '+':
			newLines = append(newLines, line.Content)
		}
	}

	input, err := json.Marshal(editHookInput{
		FilePath:   path,
		OldString:  strings.Join(oldLines, "\n"),
		NewString:  strings.Join(newLines, "\n"),
		ReplaceAll: false,
	})
	if err != nil {
		return tool.HookCall{}, fmt.Errorf("encode Edit hook input: %w", err)
	}

	return tool.HookCall{
		ToolID: "edit",
		Name:   editHookName,
		Input:  input,
	}, nil
}
