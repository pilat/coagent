package localapi

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/sessionevent"
)

const (
	tailBatch    = 100
	tailFallback = 5 * time.Second
)

type outputService struct{}

type tail struct {
	author  *client
	stream  coagentv1connect.OutputServiceTailOutputServerStream
	events  *tailEvents
	current int64
	cursor  int64
	read    bool
	state   sessionevent.State
}

func (outputService) TailOutput(ctx context.Context, req *coagentv1.TailOutputRequest,
	stream coagentv1connect.OutputServiceTailOutputServerStream,
) error {
	author, err := clientFrom(ctx)
	if err != nil {
		return err
	}

	if req.GetSessionId() <= 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "session_id must be positive")
	}

	if req.GetAfterId() < 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "after_id must be non-negative")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := author.control.Subscribe()
	defer author.control.Unsubscribe(events)

	drain := &tailEvents{wake: make(chan struct{}, 1)}
	go drain.run(ctx, events)

	t := &tail{author: author, stream: stream, events: drain, current: req.GetSessionId(), cursor: req.GetAfterId()}

	return t.run(ctx)
}

func (t *tail) run(ctx context.Context) error {
	for {
		killed, err := t.killed(ctx)
		if err != nil {
			return err
		}

		next, err := t.successor(ctx)
		if err != nil {
			return err
		}

		if next != 0 {
			if err := t.switchTo(ctx, next); err != nil {
				return err
			}

			continue
		}

		sent, err := t.sendPage(ctx, 0)
		if err != nil {
			return err
		}

		if sent {
			continue
		}

		if killed {
			// The page read after killed_at has drained the closing row.
			return nil
		}

		if err := t.wait(ctx); err != nil {
			return err
		}
	}
}

func (t *tail) sendPage(ctx context.Context, before int64) (bool, error) {
	rows, err := t.author.chat.ChatOutputs(ctx, "", t.current, t.cursor, tailBatch)
	if err != nil {
		return false, t.readError(ctx, err)
	}

	t.read = true
	sent := false

	for _, row := range rows {
		if before != 0 && row.ID >= before {
			break
		}

		err := t.stream.Send(&coagentv1.TailOutputResponse{
			Event: &coagentv1.TailOutputResponse_Output{Output: outputRow(row)},
		})
		if err != nil {
			return false, fmt.Errorf("send tail event: %w", err)
		}

		t.cursor = row.ID
		sent = true
	}

	return sent, nil
}

func (t *tail) successor(ctx context.Context) (int64, error) {
	next, err := t.author.lineage.Successor(ctx, t.current)
	if err != nil {
		return 0, t.readError(ctx, err)
	}

	return next, nil
}

func (t *tail) switchTo(ctx context.Context, next int64) error {
	rows, err := t.author.chat.ChatOutputs(ctx, "", next, 0, 1)
	if err != nil {
		return t.readError(ctx, err)
	}

	if len(rows) == 0 {
		return connect.Errorf(connect.CodeUnavailable, "session %d has no output yet", next)
	}

	t.read = true

	for {
		sent, err := t.sendPage(ctx, rows[0].ID)
		if err != nil {
			return err
		}

		if !sent {
			break
		}
	}

	t.current = next
	t.state = ""

	return nil
}

func (t *tail) killed(ctx context.Context) (bool, error) {
	killed, err := t.author.chat.SessionKilled(ctx, t.current)
	if err != nil {
		return false, t.readError(ctx, err)
	}

	return killed, nil
}

func (t *tail) wait(ctx context.Context) error {
	timer := time.NewTimer(tailFallback)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return connect.Errorf(connect.CodeUnavailable, "the local API is closing")
	case <-timer.C:
		return nil
	case <-t.events.wake:
	}

	state, ok := t.events.takeState(t.current)
	if !ok || state == t.state {
		return nil
	}

	t.state = state
	if err := t.stream.Send(&coagentv1.TailOutputResponse{
		Event: &coagentv1.TailOutputResponse_State{State: &coagentv1.SessionState{
			SessionId: t.current, State: runtimeState(state),
		}},
	}); err != nil {
		return fmt.Errorf("send tail event: %w", err)
	}

	return nil
}

func (t *tail) readError(ctx context.Context, err error) error {
	if ctx.Err() != nil || !t.read {
		return refusal(ctx, err)
	}

	return connect.Errorf(connect.CodeUnavailable, "%s", err.Error())
}
