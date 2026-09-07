package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// receiptBoundary models the durable inbox receipt contract: the receipt row is
// persisted by the acceptance transaction itself, and only a committed row
// grants a receipt id.
type receiptBoundary struct {
	input *PendingInput

	accepts         int
	receipts        []string
	receiptID       int64
	rejections      []string
	promotedContent string
	blocked         bool
}

func (b *receiptBoundary) Peek(context.Context) (*PendingInput, error) { return b.input, nil }

func (b *receiptBoundary) AcceptWithReceipt(
	_ context.Context,
	_ PendingInput,
	prepared string,
	_ []PendingToolCall,
	receipt string,
) (bool, bool, int64, error) {
	b.accepts++
	b.promotedContent = prepared
	if b.blocked {
		return false, true, 0, nil
	}

	b.receipts = append(b.receipts, receipt)
	b.receiptID += 100
	b.input = nil

	return true, false, b.receiptID, nil
}

func (b *receiptBoundary) Accept(
	_ context.Context,
	_ PendingInput,
	_ string,
	_ []PendingToolCall,
) (bool, bool, error) {
	b.accepts++
	b.input = nil

	return true, false, nil
}

func (b *receiptBoundary) Reject(_ context.Context, _ PendingInput, reason string) error {
	b.rejections = append(b.rejections, reason)
	b.input = nil

	return nil
}

func (b *receiptBoundary) Handle(context.Context, PendingInput, string) error {
	b.input = nil

	return nil
}

// A manager-owned /skill expansion persists its activation receipt inside the
// acceptance transition: exactly one receipt, keyed by the accepted input, and
// the ephemeral notification only after that commit.
func TestRunLoopAcceptsSkillWithActivationReceipt(t *testing.T) {
	agent := newTestAgent()
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{
		Name:        "review",
		Description: "Review changes",
		Content:     "Review $ARGUMENTS carefully.",
	})
	agent.loader = ldr

	boundary := &receiptBoundary{
		input: &PendingInput{
			ID: 1, Content: "/skill review  current diff",
			ManagerOwned: true, ReceivedAt: time.Now(),
		},
	}
	agent.boundary = boundary

	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("answered")}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	require.Equal(t, 1, boundary.accepts)
	require.Len(t, boundary.receipts, 1)
	assert.Equal(t, "🔧 Activated skill: review", boundary.receipts[0])
	assert.Contains(t, boundary.promotedContent, "<skill>\n<name>review</name>")
	assert.Equal(t, 1, notifier.countWith("🔧 Activated skill: review"))
	assert.Equal(t, 1, notifier.countWith("answered"), "the answer follows the receipt")
}

// The receipt belongs to the manager-owned root path. A non-manager input that
// expands a skill promotes through the plain Accept path with no receipt.
func TestRunLoopSkillWithoutManagerOwnerHasNoReceipt(t *testing.T) {
	agent := newTestAgent()
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "review", Content: "Review changes."})
	agent.loader = ldr

	boundary := &receiptBoundary{
		input: &PendingInput{ID: 1, Content: "/skill review", ReceivedAt: time.Now()},
	}
	agent.boundary = boundary

	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("answered")}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Equal(t, 1, boundary.accepts)
	assert.Empty(t, boundary.receipts, "non-manager input resolves no activation receipt")
	assert.Zero(t, notifier.countWith("🔧 Activated skill"))
}

// An unavailable skill is rejected before acceptance: no transcript row, no
// receipt, and the human is told through the existing rejection notice.
func TestRunLoopUnknownSkillEmitsNoReceipt(t *testing.T) {
	agent := newTestAgent()
	agent.loader = loader.New()

	boundary := &receiptBoundary{
		input: &PendingInput{
			ID: 1, Content: "/skill nope", ManagerOwned: true, ReceivedAt: time.Now(),
		},
	}
	agent.boundary = boundary

	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("answered")}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Zero(t, boundary.accepts)
	assert.Empty(t, boundary.receipts)
	require.Len(t, boundary.rejections, 1)
	assert.Contains(t, boundary.rejections[0], "skill unavailable: nope")
	assert.Equal(t, 1, notifier.countWith("⚠️ skill unavailable: nope"))
}

// A blocked promotion (pending external work) must end the drain like the
// plain Accept path: the durable input stays put and peeking again would spin.
func TestRunLoopBlockedSkillPromotionEndsTheDrain(t *testing.T) {
	agent := newTestAgent()
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "review", Content: "Review changes."})
	agent.loader = ldr

	boundary := &receiptBoundary{
		blocked: true,
		input: &PendingInput{
			ID: 1, Content: "/skill review", ManagerOwned: true, ReceivedAt: time.Now(),
		},
	}
	agent.boundary = boundary

	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("answered")}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	require.NotNil(t, result)
	// Pre-fix, the drain re-peeked the blocked input forever and never
	// returned; any finite completion with no receipt is the regression proof.
	assert.GreaterOrEqual(t, boundary.accepts, 1)
	assert.Empty(t, boundary.receipts)
	assert.Zero(t, notifier.countWith("🔧 Activated skill"))
}

// Receipt content never carries skill arguments: whitespace and free-form
// arguments may shape the prompt, never the displayed canonical name.
func TestPrepareUserMessageDetailedReceiptIdentity(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "review", Content: "Review $ARGUMENTS."})

	s := newMockSvc(t, nil, "")
	s.loader = ldr

	prepared, err := s.PrepareUserMessageDetailed(" \t/skill\treview  first\n  second  ")
	require.NoError(t, err)
	assert.Equal(t, "review", prepared.SkillName)
	assert.Equal(t, builtin.SkillReceipt("review"), builtin.SkillReceipt(prepared.SkillName))
	assert.Contains(t, prepared.Content, "Review first\n  second.")

	ordinary, err := s.PrepareUserMessageDetailed("plain text")
	require.NoError(t, err)
	assert.Empty(t, ordinary.SkillName)
	assert.Equal(t, "plain text", ordinary.Content)
}

var _ ReceiptBoundary = (*receiptBoundary)(nil)
