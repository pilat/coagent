package config

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

	ToolsConfig struct {
		Bash BashToolConfig `yaml:"bash,omitempty"`
	}

	// UnifiedConfig represents the unified configuration file (~/.coagent/config.yaml)
	UnifiedConfig struct {
		Providers      map[string]ProviderEntry `yaml:"providers,omitempty"`
		Marketplaces   []MarketplaceEntry       `yaml:"marketplaces,omitempty"`
		Models         []ModelEntry             `yaml:"models,omitempty"`
		SpawnFavorites []string                 `yaml:"spawn_favorites,omitempty"`
		ProjectsRoot   string                   `yaml:"projects_root,omitempty"` // root for /new folder-projects; empty → ~/.coagent/projects (resolved in daemon)
		Managers       []ManagerEntry           `yaml:"managers,omitempty"`
		Tools          ToolsConfig              `yaml:"tools,omitempty"`
	}
)
