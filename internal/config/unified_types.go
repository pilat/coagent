package config

// Search provider names accepted by tools.search.provider.
const (
	SearchProviderTavily  = "tavily"
	SearchProviderSearxng = "searxng"
)

// The yaml tags carry omitempty throughout: config.yaml is machine-written once
// a config tool touches it, and a file padded with empty defaults is one a human
// can no longer read.
type (
	MarketplaceEntry struct {
		URL     string   `yaml:"url"`
		Plugins []string `yaml:"plugins,omitempty"`
	}

	// ProviderEntry defines an LLM provider with its driver and credentials.
	ProviderEntry struct {
		Driver  string `yaml:"driver"`             // required: "anthropic", "openai", "google-sa"
		APIKey  string `yaml:"api_key,omitempty"`  // for anthropic, openai drivers
		SAFile  string `yaml:"sa_file,omitempty"`  // for google-sa driver
		BaseURL string `yaml:"base_url,omitempty"` // optional for openai, required for google-sa
		Catalog string `yaml:"catalog,omitempty"`  // models.dev section to resolve models against; driver default when empty
	}

	// ModelEntry is a switchable model. The YAML carries intent; everything below
	// TimeoutSec is catalog-filled at startup.
	ModelEntry struct {
		ID               string            `json:"id"                yaml:"id"`
		Provider         string            `json:"provider"          yaml:"provider"` // required: references providers map key
		Tags             []string          `json:"tags,omitempty"    yaml:"tags,omitempty"`
		TimeoutSec       int               `json:"timeout_sec"       yaml:"timeout_sec,omitempty"` // per-request timeout for the model; 0 = 10m default
		OpenRouterConfig *OpenRouterConfig `json:"openrouter_config" yaml:"openrouter_config,omitempty"`

		Name          string         `json:"name"              yaml:"-"`
		DisplayName   string         `json:"-"                 yaml:"-"`
		ContextWindow int            `json:"context_window"    yaml:"-"`
		MaxTokens     int            `json:"max_tokens"        yaml:"-"`
		Pricing       *ModelPricing  `json:"pricing,omitempty" yaml:"-"`
		Reasoning     *ReasoningSpec `json:"-"                 yaml:"-"`

		// InputModalities is the catalog's declared input types ("text", "image",
		// ...). Nil means unknown — capability gates must fail closed.
		InputModalities []string `json:"-" yaml:"-"`

		// EffortLevels is what the picker offers for this model — the catalog's
		// allowlist narrowed to what the driver can actually deliver. Empty = no step.
		EffortLevels []string `json:"-" yaml:"-"`
		// DefaultEffort is the level a switch to this model lands on.
		DefaultEffort string `json:"-" yaml:"-"`
	}

	// ReasoningSpec is a model's catalog-declared reasoning capability.
	ReasoningSpec struct {
		Supported    bool // model reasons at all
		NativeEffort bool // takes an effort level directly, rather than a token budget
		BudgetMin    int  // minimum budget_tokens for budget-based models; 0 when N/A

		// Efforts is the catalog's allowlist, highest-effort-first order not assumed.
		// Nil with AnyEffort false means the model exposes no effort selector at all.
		Efforts   []string
		AnyEffort bool   // catalog declares no allowlist: every gateway level is accepted
		Default   string // catalog's preferred level; empty when it declares none
	}

	// OpenRouterConfig holds OpenRouter-specific provider configuration.
	OpenRouterConfig struct {
		Only  []string `json:"only"  yaml:"only,omitempty"`
		Order []string `json:"order" yaml:"order,omitempty"`
	}

	// ModelPricing is a model's catalog-resolved cost. All prices are USD per 1M tokens.
	ModelPricing struct {
		InputPrice      float64 `json:"input_price"       yaml:"input_price"`
		OutputPrice     float64 `json:"output_price"      yaml:"output_price"`
		CacheReadPrice  float64 `json:"cache_read_price"  yaml:"cache_read_price"`
		CacheWritePrice float64 `json:"cache_write_price" yaml:"cache_write_price"`
	}

	ManagerWhisperEntry struct {
		Provider string `yaml:"provider"`
		Model    string `yaml:"model"`
	}

	ManagerEntry struct {
		ID      string `yaml:"id"`
		Driver  string `yaml:"driver"`
		Enabled *bool  `yaml:"enabled,omitempty"`

		BotToken                string  `yaml:"bot_token,omitempty"`
		AllowedUserIDs          []int64 `yaml:"allowed_user_ids,omitempty"`
		TargetChatID            *int64  `yaml:"target_chat_id,omitempty"`
		ServiceTopicName        string  `yaml:"service_topic_name,omitempty"`
		ServiceTopicIconEmojiID string  `yaml:"service_topic_icon_emoji_id,omitempty"`
		SessionTopicIconEmojiID string  `yaml:"session_topic_icon_emoji_id,omitempty"`
		SendChunkDelayMS        int     `yaml:"send_chunk_delay_ms,omitempty"`
		PollTimeoutSec          int     `yaml:"poll_timeout_sec,omitempty"`

		Whisper *ManagerWhisperEntry `yaml:"whisper,omitempty"`
	}

	BashSandboxConfig struct {
		Enabled       bool     `yaml:"enabled,omitempty"`
		WritablePaths []string `yaml:"writable_paths,omitempty"`
	}

	BashToolConfig struct {
		Sandbox BashSandboxConfig `yaml:"sandbox,omitempty"`
	}

	// SearchToolConfig configures the builtin websearch tool. An empty section
	// means unconfigured: no builtin tool, native passthrough if the driver
	// supports it. Explicitly enabled=false switches integrated search off.
	SearchToolConfig struct {
		Provider   string `yaml:"provider,omitempty"` // SearchProviderTavily | SearchProviderSearxng
		APIKey     string `yaml:"api_key,omitempty"`  // tavily credential; ${VAR} references resolved from secrets
		BaseURL    string `yaml:"base_url,omitempty"` // searxng instance root, e.g. https://searx.example.com
		MaxResults int    `yaml:"max_results,omitempty"`
		// Enabled is tri-state: nil = follow defaults, false = all integrated
		// search off, true = require an explicit provider.
		Enabled *bool `yaml:"enabled,omitempty"`
	}

	ToolsConfig struct {
		Bash   BashToolConfig   `yaml:"bash,omitempty"`
		Search SearchToolConfig `yaml:"search,omitempty"`
	}

	// UnifiedConfig represents the unified configuration file (~/.coagent/config.yaml)
	UnifiedConfig struct {
		Providers      map[string]ProviderEntry `yaml:"providers,omitempty"`
		Marketplaces   []MarketplaceEntry       `yaml:"marketplaces,omitempty"`
		Models         []ModelEntry             `yaml:"models,omitempty"`
		SpawnFavorites []string                 `yaml:"spawn_favorites,omitempty"`
		ProjectsRoot   string                   `yaml:"projects_root,omitempty"`  // root for /new folder-projects; empty → ~/.coagent/projects (resolved in daemon)
		WorktreesRoot  string                   `yaml:"worktrees_root,omitempty"` // root for /gwt worktrees; empty → ~/.coagent/worktrees (resolved in daemon)
		Managers       []ManagerEntry           `yaml:"managers,omitempty"`
		Tools          ToolsConfig              `yaml:"tools,omitempty"`
	}
)

// SearchActive reports whether the section selects the builtin REST search
// tool: a provider set and not explicitly disabled.
func (s SearchToolConfig) SearchActive() bool {
	return s.Provider != "" && (s.Enabled == nil || *s.Enabled)
}

// SearchDisabled reports whether integrated search is explicitly switched off —
// the builtin REST tool and the drivers' native passthrough both stay off.
func (s SearchToolConfig) SearchDisabled() bool {
	return s.Enabled != nil && !*s.Enabled
}

// SearchNativeActive reports whether integrated search falls back to the
// driver's native passthrough for the named model: nothing explicitly
// configured in tools.search and the model's driver executes searches
// server-side (openrouter only today).
func (c *UnifiedConfig) SearchNativeActive(model string) bool {
	if c == nil {
		return false
	}

	search := c.Tools.Search
	if search.SearchDisabled() || search.SearchActive() {
		return false
	}

	modelEntry, ok := c.findModel(model)
	if !ok {
		return false
	}

	provider, ok := c.Providers[modelEntry.Provider]
	if !ok {
		return false
	}

	return provider.Driver == driverOpenRouter
}

func (c *UnifiedConfig) findModel(id string) (ModelEntry, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}

	return ModelEntry{}, false
}
