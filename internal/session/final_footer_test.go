package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestFinalFooterStoredContent(t *testing.T) {
	for _, omit := range []bool{false, true} {
		name := "ordinary footer"
		if omit {
			name = "clean model text"
		}
		t.Run(name, func(t *testing.T) {
			db, store, id := newAttachmentsStore(t)
			_, err := store.Enqueue(t.Context(), sessionstore.Input{
				SessionID: id, Source: sessionstore.InputSourceUser, Content: "answer please",
			})
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			prompt := sessionprompt.NewBuilder("test", "")
			prompt.Todos = todo.New()
			client := &loopScriptLLM{
				responses: []*llmwire.Response{textResponse("complete answer"), textResponse("done")},
			}
			s, err := New(t.Context(), Input{
				Record: record, Client: client, Loader: loader.New(), Registry: tool.NewRegistry(),
				Prompt: prompt, Store: store, OutputEnabled: true, OmitFinalFooter: omit,
			})
			require.NoError(t, err)
			t.Cleanup(s.Close)
			_, err = s.Run(t.Context())
			require.NoError(t, err)
			var content string
			require.NoError(t, db.QueryRowContext(t.Context(),
				`SELECT content FROM session_outbox WHERE session_id = ? AND source_key LIKE 'message:%:final'`, id,
			).Scan(&content))
			if omit {
				assert.Equal(t, "complete answer", content)
			} else {
				assert.Contains(t, content, "complete answer")
				assert.NotEqual(t, "complete answer", content)
				assert.Contains(t, content, "model")
			}
		})
	}
}
