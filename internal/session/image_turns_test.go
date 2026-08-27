package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// The end-to-end pipeline per ADR-0034, driven through production components:
// a synthetic metadata turn carries no refs anywhere (it never crossed
// Telegram's string boundary), the model calls read on the path, the read tool
// returns an ImageRef, the ref persists on the role-tool row, and the wire a
// real driver sends carries pixels for a vision catalog but placeholders for a
// text-only one.
func TestImageTurns_ReadThroughDriverProjection(t *testing.T) {
	ctx := context.Background()

	workDir := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13}
	imagePath := filepath.Join(workDir, "image.png")
	require.NoError(t, os.WriteFile(imagePath, png, 0o600))

	stack, err := builtin.BuildStack(ctx, builtin.StackConfig{WorkDir: workDir, Loader: loader.New(), Todo: todo.New()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })

	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	res, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	sess, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	s, _ := newImagePlumbAgent(t)
	s.registry = stack.Registry
	s.ms = newMessageStore(store, sess.ID)

	// The synthetic upload turn is plain text everywhere — no refs exist yet.
	require.NoError(t, s.ms.addUserMessage(ctx,
		"The user attached a file:\n- name: photo.jpg\n- size: 12B\n- path: "+imagePath+
			"\n\nUse the read tool on this path to view the image."))

	calls := []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"file_path":"image.png"}`)}}
	require.NoError(t, s.ms.addAssistantMessage(
		ctx,
		&llmwire.Response{Text: "", ToolCalls: calls},
	))
	require.NoError(t, executeToolCalls(ctx, s, calls))

	require.NoError(t, s.ms.reloadMessages(ctx))
	msgs := s.ms.getMessages()

	require.Len(t, msgs[2].Images, 1, "the read result row persists its image ref")
	ref := msgs[2].Images[0]
	assert.Equal(t, imagePath, ref.Path)
	assert.Equal(t, llmwire.MimeImagePng, ref.Mime)
	assert.Empty(t, msgs[1].Images, "the synthetic upload turn itself never carries refs")

	for _, tt := range []struct {
		name         string
		modalities   []string
		wantB64      bool
		wantNoPixels bool
	}{
		{name: "vision model receives pixels", modalities: []string{"text", "image"}, wantB64: true},
		{name: "text-only model degrades", modalities: nil, wantNoPixels: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex

			var body string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)

				mu.Lock()
				body = string(raw)
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`))
			}))
			t.Cleanup(server.Close)

			cfg := &config.Config{Model: "m"}
			cfg.UnifiedConfig = &config.UnifiedConfig{
				Providers: map[string]config.ProviderEntry{
					"p": {Driver: "openai", APIKey: "key", BaseURL: server.URL},
				},
				Models: []config.ModelEntry{{
					ID: "m", Provider: "p", MaxTokens: 128, ContextWindow: 100000,
					InputModalities: tt.modalities,
				}},
			}

			clientV, err := llm.NewClientWithModel(cfg, "m")
			require.NoError(t, err)

			resp, chatErr := clientV.Chat(ctx, "", msgs, nil)
			require.NoError(t, chatErr)
			assert.Equal(t, "ok", resp.Text)

			mu.Lock()
			defer mu.Unlock()

			if tt.wantB64 {
				assert.Contains(t, body, base64.StdEncoding.EncodeToString(png))
			}

			if tt.wantNoPixels {
				assert.NotContains(t, body, "image_url")
				assert.Contains(t, body, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision))
			}
		})
	}
}
