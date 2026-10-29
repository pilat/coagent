package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
)

func TestHarnessScenario_AssistantTargetsBudgetUnderItsOwnGrant(t *testing.T) {
	for _, tc := range []struct {
		name          string
		grant, target bool
	}{
		{"target", true, true}, {"without grant", false, true}, {"own root", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var workID int64
			release := make(chan struct{})
			h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasToolResultFor(messages, budget.ToolID) {
					<-release
					return textReply("budget request handled")
				}
				params := `{"action":"set","duration":"1m"}`
				if tc.target {
					params = fmt.Sprintf(`{"action":"set","duration":"1m","session":"#%d"}`, workID)
				}
				return callReply("budget-work", budget.ToolID, params)
			}})
			defer func() {
				closeOnce(release)
				h.shutdown()
			}()
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			workID = workWithAnswer(t, h, root.ID, false, "work", "answer")
			h.startInboxWake()
			input := fmt.Sprintf("budget #%d for one minute", workID)
			if tc.grant {
				input = fmt.Sprintf("/budget #%d 1m", workID)
			}
			require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, input))
			h.waitUntil("assistant budget result", func() bool {
				return countToolResultsFor(h.messages(root.ID), budget.ToolID) == 1
			})
			if !tc.grant {
				assert.Contains(t, configLastToolResultContent(h.messages(root.ID), budget.ToolID),
					"This change requires a current user message beginning with /budget.")
				_, loadErr := h.store.Get(h.ctx, workID)
				require.ErrorIs(t, loadErr, budget.ErrNotFound)
				return
			}
			target, prefix := root.ID, "Budget armed:"
			if tc.target {
				target, prefix = workID, fmt.Sprintf("Budget armed for #%d:", workID)
			}
			record, err := h.store.Get(h.ctx, target)
			require.NoError(t, err)
			assert.Equal(t, budget.Armed, record.State)
			require.NotNil(t, record.DurationSeconds)
			assert.Equal(t, int64(60), *record.DurationSeconds)
			receipts := 0
			for _, row := range h.outbox(root.ID) {
				if strings.HasPrefix(row.Content, prefix) {
					receipts++
				}
			}
			assert.Equal(t, 1, receipts)
			for _, row := range h.outbox(workID) {
				assert.NotContains(t, row.Content, "Budget armed")
			}
			if tc.target {
				_, loadErr := h.store.Get(h.ctx, root.ID)
				require.ErrorIs(t, loadErr, budget.ErrNotFound)
			}
			closeOnce(release)
			h.waitUntil("assistant finishes budget turn", func() bool {
				return assistantSayCount(t, h, root.ID, "budget request handled") == 1 && !h.mgr.HasActiveLoop(root.ID)
			})
			if !tc.target {
				record, err = h.store.Get(h.ctx, root.ID)
				require.NoError(t, err)
				assert.Equal(t, budget.Released, record.State)
			}
		})
	}
}
