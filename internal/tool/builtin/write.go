package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/tool"
)

const writeDescription = `Writes a file to the filesystem.

CRITICAL: If this is an existing file, you MUST use the Read tool first to read the file's contents. This tool will fail if you did not read the file first.

Usage:
- This tool will overwrite the existing file if there is one at the provided path
- Creates parent directories if they don't exist
- The file_path parameter can be absolute or relative to the working directory
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required
- Use for creating new files; prefer Edit tool for modifying existing files`

var _ tool.Tool = (*writeTool)(nil)

type writeParams struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type writeTool struct {
	workDir string
	lspMgr  lsp.Manager
	mutator fileMutator
	access  safefile.Access
}

func newWriteToolWithAccess(
	workDir string,
	access safefile.Access,
	lspMgr lsp.Manager,
	mutator fileMutator,
) *writeTool {
	return &writeTool{workDir: workDir, access: access, lspMgr: lspMgr, mutator: mutator}
}

func newWriteTool(workDir string, lspMgr lsp.Manager, mutator fileMutator) *writeTool {
	return &writeTool{workDir: workDir, lspMgr: lspMgr, mutator: mutator}
}

func (t *writeTool) ID() string          { return "write" }
func (t *writeTool) ParallelSafe() bool  { return false }
func (t *writeTool) Description() string { return writeDescription }

func (t *writeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "The absolute path to the file to write (must be absolute, not relative)"
			},
			"content": {
				"type": "string",
				"description": "The content to write to the file"
			}
		},
		"required": ["file_path", "content"]
	}`)
}

//nolint:wsl_v5 // Target authorization precedes every mutation side effect.
func (t *writeTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.write")

	p, err := parseWriteParams(params, log)
	if err != nil {
		return nil, err
	}

	log.Debug("executing", zap.String("filePath", p.FilePath), zap.Int("contentLength", len(p.Content)))
	filePath, err := resolveAccessTarget(t.access, t.workDir, p.FilePath)
	if err != nil {
		return nil, err
	}

	isNew, err := t.writeFile(ctx, filePath, p.Content, log)
	if err != nil {
		return nil, err
	}

	return t.writeResult(ctx, filePath, p.Content, isNew, log)
}

func (t *writeTool) writeResult(
	ctx context.Context,
	filePath, content string,
	isNew bool,
	log *zap.Logger,
) (*tool.Result, error) {
	action := "updated"
	if isNew {
		action = "created"
	}

	log.Debug("complete",
		zap.String("filePath", filePath),
		zap.String("action", action),
		zap.Int("bytesWritten", len(content)),
	)

	title := filePath
	if t.workDir != "" {
		if rel, err := filepath.Rel(t.workDir, filePath); err == nil {
			title = rel
		}
	}

	output := fmt.Sprintf("File %s successfully: %s (%d bytes)", action, filePath, len(content))

	diagnostics, err := t.writeLSPDiagnostics(ctx, filePath, log)
	if err != nil {
		return nil, err
	}

	output += diagnostics

	log.Info("file_written",
		zap.String("filePath", filePath),
		zap.String("action", action),
		zap.Int("bytes", len(content)),
	)

	return &tool.Result{
		Title:  title,
		Output: output,
		Metadata: map[string]any{
			metaKeyPath: filePath,
			"bytes":     len(content),
			"isNew":     isNew,
			"action":    action,
		},
	}, nil
}

func parseWriteParams(params json.RawMessage, log *zap.Logger) (writeParams, error) {
	var parsed writeParams
	if err := json.Unmarshal(params, &parsed); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))
		return writeParams{}, fmt.Errorf("invalid parameters: %w", err)
	}

	if parsed.FilePath == "" {
		log.Warn("empty_filepath")
		return writeParams{}, errors.New("file_path is required")
	}

	return parsed, nil
}

//nolint:wsl_v5 // Rooted metadata and sandboxed replacement stay under one file lock.
func (t *writeTool) writeFile(
	ctx context.Context,
	filePath, content string,
	log *zap.Logger,
) (bool, error) {
	unlock := lockFileWrite(filePath)
	defer unlock()

	var info os.FileInfo
	var statErr error
	if t.access != nil && t.access.Scope() == safefile.ProjectConfined {
		info, _, statErr = t.access.Stat(filePath)
	} else {
		info, statErr = os.Stat(filePath)
	}
	if statErr == nil && info.IsDir() {
		return false, fmt.Errorf("path is a directory, not a file: %s", filePath)
	}
	isNew := errors.Is(statErr, os.ErrNotExist)

	if err := t.mutator.WriteFile(ctx, filePath, []byte(content), true); err != nil {
		log.Warn("write_failed", zap.String("filePath", filePath), zap.Error(err))
		return false, fmt.Errorf("write file: %w", err)
	}

	return isNew, nil
}

func (t *writeTool) writeLSPDiagnostics(ctx context.Context, filePath string, log *zap.Logger) (string, error) {
	if t.lspMgr == nil {
		return "", nil
	}

	log.Debug("write: checking LSP diagnostics",
		zap.String("file", filePath),
		zap.String("workDir", t.workDir),
	)

	if _, err := t.lspMgr.GetDiagnostics(ctx, t.workDir, filePath); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		log.Debug("write: LSP diagnostics unavailable", zap.Error(err))

		return "", nil
	}

	diagnostics := t.lspMgr.GetAllDiagnostics(ctx, t.workDir, 20, 5)
	log.Debug("write: LSP diagnostics",
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
