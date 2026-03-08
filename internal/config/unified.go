package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pilat/coagent/internal/coagenthome"
)

const (
	// DefaultUnifiedConfigFile is the default path for unified config.
	DefaultUnifiedConfigFile = "~/" + coagenthome.DirName + "/" + coagenthome.ConfigFileName

	defaultTelegramServiceTopicName        = "Coagent"
	defaultTelegramServiceTopicIconEmojiID = "5309832892262654231"
	defaultTelegramSessionTopicIconEmojiID = "5312016608254762256"
	defaultTelegramSendChunkDelayMS        = 100
	defaultTelegramPollTimeoutSec          = 30

	driverAnthropic  = "anthropic"
	driverOpenAI     = "openai"
	driverOpenRouter = "openrouter"
	driverGoogleSA   = "google-sa"
)

var (
	// validDrivers is the set of supported provider drivers.
	validDrivers = map[string]bool{
		driverAnthropic:  true,
		driverOpenAI:     true,
		driverOpenRouter: true,
		driverGoogleSA:   true,
	}

	// validManagerDrivers is the set of supported embedded manager drivers.
	validManagerDrivers = map[string]bool{
		"telegram": true,
	}
)

// ExpandPath resolves a leading ~/ against the user's home directory. Config
// paths are written the way a human types them; everything downstream needs the
// absolute form.
func ExpandPath(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}

	homeDir, err := coagenthome.UserHome()
	if err != nil {
		return "", fmt.Errorf("getting home directory: %w", err)
	}

	return filepath.Join(homeDir, path[2:]), nil
}

// LoadUnifiedConfig reads the YAML config and resolves ${VAR} references from
// secrets. Only the credential-bearing fields accept a reference; see
// resolveSecrets.
func LoadUnifiedConfig(configPath string, secrets Secrets) (*UnifiedConfig, error) {
	data, err := readConfigFile(configPath)
	if err != nil {
		return nil, err
	}

	return ParseAndResolve(data, secrets)
}

// LoadRawUnifiedConfig reads the YAML config *without* resolving secrets, so the
// draft keeps its ${VAR} references. Every mutation starts from this form:
// rendering a resolved config back would write credentials into config.yaml in
// plaintext.
func LoadRawUnifiedConfig(configPath string) (*UnifiedConfig, error) {
	data, err := readConfigFile(configPath)
	if err != nil {
		return nil, err
	}

	return ParseUnifiedConfig(data)
}

// ParseUnifiedConfig decodes config bytes without touching secrets. Unknown
// fields are fatal: a typo that silently disables a section is worse than a
// refusal.
func ParseUnifiedConfig(data []byte) (*UnifiedConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var cfg UnifiedConfig
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}

		return nil, errors.New("parsing config file: multiple YAML documents are not supported")
	}

	return &cfg, nil
}

// ParseAndResolve decodes bytes, expands ${VAR} from secrets, and validates. It
// is the gate a candidate config must pass before it may replace the live file —
// validating the bytes first is what keeps a bad write from ever landing.
func ParseAndResolve(data []byte, secrets Secrets) (*UnifiedConfig, error) {
	cfg, err := ParseUnifiedConfig(data)
	if err != nil {
		return nil, err
	}

	if err := cfg.resolveSecrets(secrets); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// MarshalUnifiedConfig renders a config back to YAML. The input must be the raw
// form: marshalling a secret-resolved config writes credentials in plaintext.
func MarshalUnifiedConfig(cfg *UnifiedConfig) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	return data, nil
}

func readConfigFile(configPath string) ([]byte, error) {
	path, err := ExpandPath(configPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	return data, nil
}

func (c *UnifiedConfig) validate() error {
	if err := c.validateManagers(); err != nil {
		return err
	}

	if err := c.validateProviders(); err != nil {
		return err
	}

	return c.validateModels()
}

func (c *UnifiedConfig) validateManagers() error {
	seen := make(map[string]struct{}, len(c.Managers))
	for i := range c.Managers {
		if err := c.validateManager(i, seen); err != nil {
			return err
		}
	}

	return nil
}

func (c *UnifiedConfig) validateManager(i int, seen map[string]struct{}) error {
	m := &c.Managers[i]

	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("manager at index %d has no id specified", i)
	}

	if _, exists := seen[m.ID]; exists {
		return fmt.Errorf("duplicate manager id %q", m.ID)
	}

	seen[m.ID] = struct{}{}

	if m.Driver == "" {
		return fmt.Errorf("manager %q has no driver specified", m.ID)
	}

	if !validManagerDrivers[m.Driver] {
		return fmt.Errorf("manager %q has unknown driver %q, must be one of: telegram", m.ID, m.Driver)
	}

	if m.Enabled == nil {
		return fmt.Errorf("manager %q requires \"enabled\" to be set", m.ID)
	}

	if m.Driver != "telegram" {
		return nil
	}

	return c.validateTelegramManager(m)
}

func (c *UnifiedConfig) validateTelegramManager(m *ManagerEntry) error {
	if m.BotToken == "" {
		return fmt.Errorf("manager %q (driver: telegram) requires \"bot_token\" to be set", m.ID)
	}

	if len(m.AllowedUserIDs) == 0 {
		return fmt.Errorf("manager %q (driver: telegram) requires \"allowed_user_ids\" to be non-empty", m.ID)
	}

	if m.TargetChatID == 0 {
		return fmt.Errorf("manager %q (driver: telegram) requires \"target_chat_id\" to be set", m.ID)
	}

	applyTelegramDefaults(m)

	if m.SendChunkDelayMS < 0 {
		return fmt.Errorf("manager %q (driver: telegram) requires \"send_chunk_delay_ms\" to be >= 0", m.ID)
	}

	if m.PollTimeoutSec < 0 {
		return fmt.Errorf("manager %q (driver: telegram) requires \"poll_timeout_sec\" to be >= 0", m.ID)
	}

	return c.validateTelegramWhisper(m)
}

func applyTelegramDefaults(m *ManagerEntry) {
	if m.ServiceTopicName == "" {
		m.ServiceTopicName = defaultTelegramServiceTopicName
	}

	if m.ServiceTopicIconEmojiID == "" {
		m.ServiceTopicIconEmojiID = defaultTelegramServiceTopicIconEmojiID
	}

	if m.SessionTopicIconEmojiID == "" {
		m.SessionTopicIconEmojiID = defaultTelegramSessionTopicIconEmojiID
	}

	if m.SendChunkDelayMS == 0 {
		m.SendChunkDelayMS = defaultTelegramSendChunkDelayMS
	}

	if m.PollTimeoutSec == 0 {
		m.PollTimeoutSec = defaultTelegramPollTimeoutSec
	}
}

func (c *UnifiedConfig) validateTelegramWhisper(m *ManagerEntry) error {
	if m.Whisper == nil {
		return nil
	}

	if m.Whisper.Provider == "" || m.Whisper.Model == "" {
		return fmt.Errorf("manager %q (driver: telegram) requires both whisper.provider and whisper.model", m.ID)
	}

	p, ok := c.Providers[m.Whisper.Provider]
	if !ok {
		return fmt.Errorf("manager %q whisper references unknown provider %q", m.ID, m.Whisper.Provider)
	}

	if p.Driver != driverOpenAI {
		return fmt.Errorf("manager %q whisper provider %q must use driver \"openai\"", m.ID, m.Whisper.Provider)
	}

	return nil
}

func (c *UnifiedConfig) validateProviders() error {
	// Models without providers is valid (no LLM configured).
	// Models WITH providers requires providers map to exist.
	if len(c.Models) > 0 && len(c.Providers) == 0 {
		return errors.New("models are configured but no providers defined")
	}

	for name, p := range c.Providers {
		if err := validateProvider(name, p); err != nil {
			return err
		}
	}

	return nil
}

func validateProvider(name string, p ProviderEntry) error {
	if p.Driver == "" {
		return fmt.Errorf("provider %q has no driver specified", name)
	}

	if !validDrivers[p.Driver] {
		return fmt.Errorf(
			"provider %q has unknown driver %q, must be one of: anthropic, openai, openrouter, google-sa",
			name,
			p.Driver,
		)
	}

	switch p.Driver {
	case driverAnthropic, driverOpenAI:
		if p.APIKey == "" {
			return fmt.Errorf("provider %q (driver: %s) requires \"api_key\" to be set", name, p.Driver)
		}
	case driverOpenRouter:
		if p.APIKey == "" {
			return fmt.Errorf("provider %q (driver: openrouter) requires \"api_key\" to be set", name)
		}

		if p.BaseURL == "" {
			return fmt.Errorf("provider %q (driver: openrouter) requires \"base_url\" to be set", name)
		}
	case driverGoogleSA:
		if p.SAFile == "" {
			return fmt.Errorf("provider %q (driver: google-sa) requires \"sa_file\" to be set", name)
		}

		if p.BaseURL == "" {
			return fmt.Errorf("provider %q (driver: google-sa) requires \"base_url\" to be set", name)
		}
	}

	return nil
}

func (c *UnifiedConfig) validateModels() error {
	for _, m := range c.Models {
		if err := c.validateModel(m); err != nil {
			return err
		}
	}

	return nil
}

func (c *UnifiedConfig) validateModel(m ModelEntry) error {
	if m.Provider == "" {
		return fmt.Errorf("model %q missing required field \"provider\"", m.ID)
	}

	if _, ok := c.Providers[m.Provider]; !ok {
		return fmt.Errorf("model %q references unknown provider %q", m.ID, m.Provider)
	}

	return nil
}
