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
	id           string
	title        string
	failed       bool
	parallelSafe bool
}

func (t *imageResultTool) ID() string          { return t.id }
func (t *imageResultTool) ParallelSafe() bool  { return t.parallelSafe }
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

// A nested failure stops later stages: the failed child contributes no refs and
// the skipped sibling fabricates none, while the successful sibling keeps its
// ref and the combined result reports the typed failure.
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

	require.Len(t, result.Images, 1, "the failed and skipped children contribute no refs")
	assert.Equal(t, "a.png", filepath.Base(result.Images[0].Path))
	assert.True(t, result.IsError, "a nested failure marks the combined result")
	assert.Contains(t, result.Output, "skipped: an earlier tool call in this turn failed")
	assert.NotContains(t, result.Output, "c.png")
}

// Successful siblings that share a parallel-safe stage keep their refs in call
// order, and the combined result stays an ordinary success.
func TestBatch_ParallelSafeSiblingsKeepRefOrder(t *testing.T) {
	a := &imageResultTool{id: "alpha", title: "a.png", parallelSafe: true}
	c := &imageResultTool{id: "gamma", title: "c.png", parallelSafe: true}

	reg := tool.NewRegistry()
	reg.Register(a)
	reg.Register(c)

	batch := NewBatchTool(reg)
	params := json.RawMessage(`{"calls":[
		{"tool":"alpha","params":{}},
		{"tool":"gamma","params":{}}
	]}`)

	result, err := batch.Execute(context.Background(), params)
	require.NoError(t, err)

	require.Len(t, result.Images, 2, "refs keep call order across a shared stage")
	assert.Equal(t, "a.png", filepath.Base(result.Images[0].Path))
	assert.Equal(t, "c.png", filepath.Base(result.Images[1].Path))
	assert.False(t, result.IsError)
}

// A typed failure keeps the attachments its real result carried — same
// contract as the native path (partial output survives the failure); only
// skipped/cancelled children fabricate nothing.
func TestBatch_TypedFailedChildKeepsItsRefs(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&imageResultTool{id: "alpha", title: "a.png", parallelSafe: true})
	reg.Register(&typedFailedImageTool{id: "beta", title: "b.png"})
	reg.Register(NewBatchTool(reg))

	batch, ok := reg.Get(tool.IDBatch).(*BatchTool)
	require.True(t, ok)

	result, err := batch.Execute(context.Background(), json.RawMessage(`{"calls":[
		{"tool":"alpha","params":{}},
		{"tool":"beta","params":{}}
	]}`))
	require.NoError(t, err)

	require.Len(t, result.Images, 2, "the typed failure keeps its own ref")
	assert.Equal(t, "a.png", filepath.Base(result.Images[0].Path))
	assert.Equal(t, "b.png", filepath.Base(result.Images[1].Path))
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "partial payload")
}

// typedFailedImageTool returns a typed Result.IsError failure carrying its own ref.
type typedFailedImageTool struct {
	id    string
	title string
}

func (t *typedFailedImageTool) ID() string          { return t.id }
func (t *typedFailedImageTool) ParallelSafe() bool  { return true }
func (t *typedFailedImageTool) Description() string { return "stub" }
func (t *typedFailedImageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *typedFailedImageTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	return &tool.Result{
		Output:  "partial payload",
		IsError: true,
		Images:  []llmwire.ImageRef{{Path: "/tmp/" + t.title, Mime: llmwire.MimeImagePng, Size: 8}},
	}, nil
}
