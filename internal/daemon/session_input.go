package daemon

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/session"
)

var errSessionInputDeferred = errors.New("session input deferred behind a pending external call")

// sessionInput is the sealed daemon-internal protocol for asynchronous work
// entering a transcript. Each variant has one semantic shape; unlike the former
// ToolNotification bag, impossible field combinations cannot be constructed.
//
//sumtype:decl
type sessionInput interface {
	validate() error
	isSessionInput()
}

type pendingCallResultInput struct {
	Call    session.PendingToolCall
	Content string
}

func (pendingCallResultInput) isSessionInput() {}

func (i pendingCallResultInput) validate() error {
	if i.Call.ID == "" || i.Call.Name == "" {
		return errors.New("pending call result requires call id and tool name")
	}

	if i.Content == "" {
		return fmt.Errorf("pending call result for %s requires content", i.Call.ID)
	}

	return nil
}

type blockingSubagentCompletionInput struct {
	ChildID       int64
	CallID        string
	ActivationSeq int64
}

func (blockingSubagentCompletionInput) isSessionInput() {}

func (i blockingSubagentCompletionInput) validate() error {
	if i.ChildID <= 0 || i.CallID == "" || i.ActivationSeq <= 0 {
		return errors.New(
			"blocking subagent completion requires positive child id, task call id, and activation sequence",
		)
	}

	return nil
}

type backgroundSubagentCompletionInput struct {
	ChildID       int64
	ActivationSeq int64
}

func (backgroundSubagentCompletionInput) isSessionInput() {}

func (i backgroundSubagentCompletionInput) validate() error {
	if i.ChildID <= 0 || i.ActivationSeq <= 0 {
		return errors.New("background subagent completion requires a positive child id and activation sequence")
	}

	return nil
}

type scheduleTickInput struct {
	DeliveryID string
	Content    string
}

func (scheduleTickInput) isSessionInput() {}

func (i scheduleTickInput) validate() error {
	if i.DeliveryID == "" || i.Content == "" {
		return errors.New("schedule tick requires delivery id and content")
	}

	return nil
}

type freshScheduleInput struct {
	DeliveryID string
	Prompt     string
}

func (freshScheduleInput) isSessionInput() {}

func (i freshScheduleInput) validate() error {
	if i.DeliveryID == "" || i.Prompt == "" {
		return errors.New("fresh schedule requires delivery id and prompt")
	}

	return nil
}

func inputResolvesExistingCall(input sessionInput) bool {
	switch input.(type) {
	case pendingCallResultInput, blockingSubagentCompletionInput:
		return true
	case backgroundSubagentCompletionInput, scheduleTickInput, freshScheduleInput:
		return false
	default:
		return false
	}
}

func inputIsScheduledTurn(input sessionInput) bool {
	switch input.(type) {
	case scheduleTickInput, freshScheduleInput:
		return true
	case pendingCallResultInput, blockingSubagentCompletionInput, backgroundSubagentCompletionInput:
		return false
	default:
		return false
	}
}

func inputSleepInterruption(input sessionInput) string {
	switch input.(type) {
	case backgroundSubagentCompletionInput:
		return "Sleep interrupted — a subagent completed."
	case scheduleTickInput, freshScheduleInput:
		return "Sleep interrupted — a scheduled task became due."
	case pendingCallResultInput, blockingSubagentCompletionInput:
		return ""
	default:
		return ""
	}
}

// queuedSessionInput separates delivery mechanics from the payload protocol.
// Async child completions need no waiter because their link is the durable retry
// ledger; schedules and exact external results wait for transcript acceptance.
//
//sumtype:decl
type queuedSessionInput interface {
	input() sessionInput
	complete(bool, error)
}

type asyncSessionInput struct {
	value sessionInput
}

func (i asyncSessionInput) input() sessionInput { return i.value }
func (asyncSessionInput) complete(bool, error)  {}

type deliveryOutcome struct {
	Applied bool
	Err     error
}

type awaitedSessionInput struct {
	value sessionInput
	done  chan deliveryOutcome
	once  sync.Once
}

func newAwaitedSessionInput(value sessionInput) *awaitedSessionInput {
	return &awaitedSessionInput{value: value, done: make(chan deliveryOutcome, 1)}
}

func (i *awaitedSessionInput) input() sessionInput { return i.value }

func (i *awaitedSessionInput) complete(applied bool, err error) {
	i.once.Do(func() { i.done <- deliveryOutcome{Applied: applied, Err: err} })
}

func pendingCall(calls []session.PendingToolCall, id, name string) bool {
	for _, call := range calls {
		if call.ID == id && call.Name == name {
			return true
		}
	}

	return false
}
