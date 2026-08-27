package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

type imageResultTool struct {
	id     string
	title  string
	failed bool
}

func (t *imageResultTool) ID() string          { return t.id }
func (t *imageResultTool) Description() string { return "stub" }
func (t *imageResultTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *imageResultTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	if t.failed {
		return nil, errStubBoom
	}

	return &tool.Result{
		Title:  t.title,
		Output: "<image>...</image>",
		Images: []llmwire.ImageRef{{Path: "/tmp/" + t.title, Mime: llmwire.MimeImagePng, Size: 8}},
	}, nil
}

var errStubBoom = assert.AnError

// A batch's refs propagate from its nested sub-results in nested call order —
// including when other children fail.
func TestBatch_PropagatesNestedImageRefsInCallOrder(t *testing.T) {
	a := &imageResultTool{id: "alpha", title: "a.png"}
	b := &imageResultTool{id: "beta", title: "b.png", failed: true}
	c := &imageResultTool{id: "gamma", title: "c.png"}

	reg := tool.NewRegistry()
	reg.Register(a)
	reg.Register(b)
	reg.Register(c)

	batch := NewBatchTool(reg)
	params := json.RawMessage(`{"calls":[
		{"tool":"alpha","params":{}},
		{"tool":"beta","params":{}},
		{"tool":"gamma","params":{}}
	]}`)

	result, err := batch.Execute(context.Background(), params)
	require.NoError(t, err)

	require.Len(t, result.Images, 2, "the failed child contributes no refs")
	assert.Equal(t, "a.png", filepath.Base(result.Images[0].Path))
	assert.Equal(t, "c.png", filepath.Base(result.Images[1].Path), "refs keep call order")
}
