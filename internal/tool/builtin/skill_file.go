package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/tool"
)

func (t *skillTool) openFile(ctx context.Context, p skillParams) (*tool.Result, error) {
	if p.Args != "" {
		return nil, errors.New("args and file cannot be combined")
	}

	if t.loader == nil {
		return nil, errors.New("skill catalog is unavailable")
	}

	skill := t.loader.GetSkill(p.Name)
	if skill == nil {
		return nil, fmt.Errorf("skill unavailable: %s", p.Name)
	}

	if skill.Origin == nil {
		return nil, fmt.Errorf("skill %s is not served by an MCP server; file applies only to MCP skills", p.Name)
	}

	opened, err := skill.Origin.Source.Open(ctx, p.File)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", p.Name, err)
	}

	content := "Binary content is not shown; use the local path."
	if utf8.Valid(opened.Data) && !bytes.ContainsRune(opened.Data, '\x00') {
		content = string(opened.Data)
	}

	return &tool.Result{
		Title:    p.Name + " " + p.File,
		Metadata: map[string]any{"skill": p.Name, "file": p.File, metaKeyPath: opened.Path},
		Output: fmt.Sprintf(
			"Opened file %s of skill %s (%d bytes, verified against the server's listing).\nLocal path: %s\n\n%s",
			p.File,
			p.Name,
			len(opened.Data),
			opened.Path,
			content,
		),
	}, nil
}
