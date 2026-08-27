package tool

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
)

// Well-known tool IDs referenced across packages (session resume, daemon injection).
const (
	IDSleep          = "sleep"
	IDSchedule       = "schedule"
	IDTask           = "task"
	IDSendToSubagent = "send_to_subagent"
	IDCompactContext = "compact_context"
	IDBatch          = "batch"
	IDSkill          = "skill"
	IDMCPAdd         = "mcp_add"
	IDMCPRemove      = "mcp_remove"
	IDMCPEnable      = "mcp_enable"
	IDMCPDisable     = "mcp_disable"
	IDMCPList        = "mcp_list"

	IDSetProvider     = "set_provider"
	IDRemoveProvider  = "remove_provider"
	IDSetManager      = "set_manager"
	IDRemoveManager   = "remove_manager"
	IDAddModel        = "add_model"
	IDRemoveModel     = "remove_model"
	IDSetDefaultModel = "set_default_model"
	IDSetModelTags    = "set_model_tags"
	IDRequestSecret   = "request_secret"
)

// externalCallTools suspend the loop awaiting an outcome produced outside it.
// Never re-executed, never stubbed by repair; only an injection resolves one.
var externalCallTools = map[string]bool{
	IDSleep:           true,
	IDTask:            true,
	IDSetProvider:     true,
	IDRemoveProvider:  true,
	IDSetManager:      true,
	IDRemoveManager:   true,
	IDAddModel:        true,
	IDRemoveModel:     true,
	IDSetDefaultModel: true,
	IDSetModelTags:    true,
	IDRequestSecret:   true,
}

// IsExternalCall reports whether a tool's pending call waits on the outside
// world rather than on the loop.
func IsExternalCall(name string) bool { return externalCallTools[name] }

// ErrSuspend is returned by tools (e.g., sleep) to signal that the agent loop
// should exit without recording the tool result. The session is checkpointed
// with the tool_call pending, and the real result is injected on resume.
var ErrSuspend = errors.New("tool requested session suspend")

// Tool is the interface that all tools must implement.
//
// WARNING: Description() and Parameters() MUST return deterministic output.
// These values are serialized into the LLM request and form part of the
// prompt cache key (Anthropic, OpenAI). Non-deterministic output (e.g. from
// iterating a map) invalidates the cache on every call, destroying cache hits.
type Tool interface {
	ID() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, params json.RawMessage) (*Result, error)
}

// RegistryBound is implemented by tools that dispatch to other tools through a
// registry. Registry views rebind them, so such a tool can never reach past the
// tool set it is served from.
type RegistryBound interface {
	Tool
	BindRegistry(reg Registry) Tool
}

// ToSchemas converts tools to their LLM wire descriptors. Order is preserved —
// it's part of the LLM prompt-cache key.
func ToSchemas(tools []Tool) []llmwire.ToolSchema {
	schemas := make([]llmwire.ToolSchema, len(tools))
	for i, t := range tools {
		schemas[i] = llmwire.ToolSchema{
			Name:        t.ID(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		}
	}

	return schemas
}

type Result struct {
	Title    string         `json:"title,omitempty"`
	Output   string         `json:"output"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Images carries referenced-not-stored pixel attachments this result
	// produced (read on a supported image); stored on the role-tool row and
	// materialized per request by the drivers. Stub/repair paths never set it.
	Images []llmwire.ImageRef `json:"images,omitempty"`
}

// Registry manages a collection of tools.
type Registry interface {
	Register(tool Tool)
	Unregister(id string) bool
	Get(id string) Tool
	List() []Tool
	IDs() []string
	Execute(ctx context.Context, id string, params json.RawMessage) (*Result, error)
	Clone() Registry
	Filter(ids []string) Registry
}

var _ Registry = (*svc)(nil)

type svc struct {
	mu    sync.RWMutex
	tools map[string]Tool // WARNING: map iteration is non-deterministic — List()/IDs() MUST sort results
}

func NewRegistry() Registry {
	return &svc{
		tools: make(map[string]Tool),
	}
}

func (s *svc) Register(tool Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bind(tool.ID(), tool)
}

func (s *svc) Unregister(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tools[id]; ok {
		delete(s.tools, id)
		return true
	}

	return false
}

func (s *svc) Get(id string) Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.tools[id]
}

func (s *svc) List() []Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tools := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, t)
	}
	// Stable sort by tool ID — critical for LLM prompt caching.
	// Non-deterministic map iteration would change tool order in the JSON request,
	// invalidating the cache key on every call.
	slices.SortFunc(tools, func(a, b Tool) int { return cmp.Compare(a.ID(), b.ID()) })

	return tools
}

func (s *svc) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.tools))
	for id := range s.tools {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	return ids
}

func (s *svc) Execute(ctx context.Context, id string, params json.RawMessage) (*Result, error) {
	tool := s.Get(id)
	if tool == nil {
		return nil, fmt.Errorf("tool not found: %s", id)
	}

	return tool.Execute(ctx, params)
}

func (s *svc) Clone() Registry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	newReg := &svc{tools: make(map[string]Tool)}
	for id, t := range s.tools {
		newReg.bind(id, t)
	}

	return newReg
}

func (s *svc) Filter(ids []string) Registry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	newReg := &svc{tools: make(map[string]Tool)}

	for _, id := range ids {
		if t, ok := s.tools[id]; ok {
			newReg.bind(id, t)
		}
	}

	return newReg
}

// bind installs t under id, rebinding registry-dispatching tools onto s so no
// holder of s can execute what s does not itself carry. Callers hold the lock.
func (s *svc) bind(id string, t Tool) {
	if bound, ok := t.(RegistryBound); ok {
		t = bound.BindRegistry(s)
	}

	s.tools[id] = t
}
