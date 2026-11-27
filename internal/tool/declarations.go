package tool

const (
	PromptGroupNone PromptGroup = iota
	PromptGroupFiles
	PromptGroupShell
	PromptGroupCodeIntelligence
	PromptGroupTaskTracking
	PromptGroupMemory
	PromptGroupSkills
	PromptGroupWeb
	PromptGroupSessions
)

// TrailArgument names the call parameter a tool trail line shows.
type TrailArgument struct {
	Param string
	// Path renders the value relative to the working directory when it is inside it.
	Path bool
}

// TrailDeclarer is a tool whose tool trail line shows an argument.
type TrailDeclarer interface { //nolint:iface // Declared by tools elsewhere, consulted by the core.
	Tool
	TrailArgument() TrailArgument
}

// PromptGroup names one line of the prompt's tool inventory. The core owns each
// group's title and position; a group needs a title there to render.
type PromptGroup int

// PromptDeclaration is what the system prompt needs to know about a tool. Its
// bytes form part of the prompt-cache key, so every field must be deterministic.
type PromptDeclaration struct {
	Group PromptGroup
	// Order positions the tool within its group; equal values fall back to ID order.
	Order       int
	Description string
	// Memory, Shell and WebFetch each switch on the prompt section that depends on them.
	Memory   bool
	Shell    bool
	WebFetch bool
	// Search lists the tool in the web search section, by ascending SearchOrder.
	Search      bool
	SearchOrder int
	// MCPServer names the MCP server whose guidance and skills reach a session through this visible tool.
	MCPServer    string
	Instructions string
}

// PromptDeclarer is a tool the system prompt describes.
type PromptDeclarer interface { //nolint:iface // Declared by tools elsewhere, consulted by the core.
	Tool
	PromptDeclaration() PromptDeclaration
}

// IsExternalCall reports whether a tool's pending call waits on the outside
// world rather than on the loop.
func IsExternalCall(name string) bool { return externalCallTools[name] }

// IsDirectOnly reports whether batch must refuse a tool.
func IsDirectOnly(candidate Tool) bool {
	_, direct := candidate.(DirectOnly)

	return direct
}
