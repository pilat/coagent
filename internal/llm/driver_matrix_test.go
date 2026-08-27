package llm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// The matrix constructs a real client for every registered driver name, so the
// assertions cover the conversion sites actually shipped per family (D5):
// anthropic-style MessageParams vs OpenAI-compatible chat bodies.

// saFile writes a syntactically valid service-account key so the google-sa
// driver's real constructor can run hermetically.
func saFile(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "sa.json")
	body := fmt.Sprintf(`{"type":"service_account","project_id":"p","private_key":%q,"client_email":"a@b.c"}`,
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func newMatrixClient(t *testing.T, driver string, model config.ModelEntry) Client {
	t.Helper()

	entry := config.ProviderEntry{
		Driver:  driver,
		APIKey:  "key",
		BaseURL: "http://localhost",
	}
	if driver == driverGoogleSA {
		entry.SAFile = saFile(t)
	}

	client, err := newDrivers(nil)[driver].NewClient(entry, model)
	require.NoError(t, err)

	return client
}

func imageConversation(pngPath string) []llmwire.Message {
	return []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "view this"},
		{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{}`)}},
		},
		{
			Role:       llmwire.RoleTool,
			Content:    "[img]",
			ToolCallID: "c1",
			ToolName:   "read",
			Images:     []llmwire.ImageRef{{Path: pngPath, Mime: llmwire.MimeImagePng, Size: 8}},
		},
	}
}

// serializeChatBody drives each client through its own request-construction
// path and returns the exact JSON body shape that reaches the provider.
func serializeChatBody(t *testing.T, client Client, msgs []llmwire.Message) string {
	t.Helper()

	switch typed := client.(type) {
	case *anthropicClient:
		params := typed.buildMessageParams("", msgs, nil, 256)
		raw, err := json.Marshal(params)
		require.NoError(t, err)

		return string(raw)
	case *openAICompatibleClient:
		req := oaiRequest{Model: typed.model, Messages: typed.convertMessages(msgs), MaxTokens: 256}
		data, err := json.Marshal(req)
		require.NoError(t, err)

		return string(data)
	default:
		t.Fatalf("unexpected client type %T", client)
	}

	return ""
}

func TestDriverMatrix_ImageInToolRoleContent(t *testing.T) {
	pngPath := filepath.Join(t.TempDir(), "coagent-x.png")
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	require.NoError(t, os.WriteFile(pngPath, png, 0o600))
	wantB64 := base64.StdEncoding.EncodeToString(png)

	vision := config.ModelEntry{
		ID:              "vision-model",
		MaxTokens:       1024,
		ContextWindow:   100000,
		InputModalities: []string{"text", "image"},
	}

	for _, tt := range []struct {
		name   string
		driver string
	}{
		{"anthropic", driverAnthropic},
		{"openai", driverOpenAI},
		{"google-sa", driverGoogleSA},
		{"openrouter", driverOpenRouter},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := serializeChatBody(t, newMatrixClient(t, tt.driver, vision), imageConversation(pngPath))

			assert.Contains(t, raw, wantB64, "%s: pixels materialized at request build", tt.driver)
			assert.NotContains(t, raw, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision),
				"%s: no degradation on a vision-capable catalog", tt.driver)
		})
	}
}

// The first image-bearing turn on any endpoint must degrade cleanly for a
// text-only catalog instead of sending blocks it would reject every turn after.
func TestDriverMatrix_TextOnlyModelNeverGetsPixels(t *testing.T) {
	pngPath := filepath.Join(t.TempDir(), "coagent-x.png")
	require.NoError(t, os.WriteFile(pngPath, []byte("png"), 0o600))

	textOnly := config.ModelEntry{ID: "text-model", MaxTokens: 1024, ContextWindow: 100000}

	for _, tt := range []struct {
		name   string
		driver string
	}{
		{"anthropic", driverAnthropic},
		{"openai", driverOpenAI},
		{"google-sa", driverGoogleSA},
		{"openrouter", driverOpenRouter},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := serializeChatBody(t, newMatrixClient(t, tt.driver, textOnly), imageConversation(pngPath))

			assert.NotContains(t, raw, "image_url", "%s sends no data-URL parts", tt.driver)
			assert.NotContains(t, raw, `"source"`, "%s sends no image sources", tt.driver)
			assert.Contains(t, raw, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision),
				"%s degrades each slot to a placeholder", tt.driver)
		})
	}
}
