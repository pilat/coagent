package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type waitingProjection struct {
	wait     sessionevent.WaitItem
	display  map[string]any
	identity map[string]any
}

var errWaitingSlotNotSuspended = errors.New("waiting slot requires a suspended root")

func (s *svc) liveContextProjection(ctx context.Context, rootID int64) (progress.Context, bool) {
	activeRunner, ok := s.runners.load(rootID)
	if !ok {
		return progress.Context{}, false
	}

	service := activeRunner.Service()
	if service == nil {
		return progress.Context{}, false
	}

	projection := service.ContextProjection(ctx)

	return progress.Context{
		Used:        projection.Used,
		Max:         projection.Max,
		Approximate: projection.Approximate,
		Available:   projection.Available,
	}, true
}

func (s *svc) mainModelWorking(ctx context.Context, rootID int64) bool {
	// The durable status outranks the runner flag: a root parked on an
	// external call owns no runner service by the time its waiting card is
	// captured, but a concurrent capture can still observe the live loop
	// before finishRunner clears it — a suspended root is never "working".
	record, err := s.store.GetSession(ctx, rootID)
	if err != nil || record.Status == sessionstore.SessionStatusSuspended {
		return false
	}

	activeRunner, ok := s.runners.load(rootID)
	if !ok {
		return false
	}

	return activeRunner.Working()
}

func (s *svc) publishSubagentProgress(ctx context.Context, childID int64) {
	s.publishSubagentProgressWithIteration(ctx, childID, nil)
}

func (s *svc) publishSubagentIterationProgress(ctx context.Context, childID, iteration int64) {
	s.publishSubagentProgressWithIteration(ctx, childID, &iteration)
}

func (s *svc) publishSubagentProgressWithIteration(
	ctx context.Context,
	childID int64,
	checkpointIteration *int64,
) {
	log := logger.Ctx(ctx).Named("daemon.progress")

	record, err := s.store.GetSession(ctx, childID)
	if err != nil {
		log.Warn("load_subagent_progress_session", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if record.ParentID == 0 {
		return
	}

	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		log.Warn("load_subagent_progress_link", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if link == nil {
		return
	}

	if checkpointIteration != nil && link.Blocking {
		return
	}

	var causalID string
	if checkpointIteration != nil {
		causalID = fmt.Sprintf(
			"subagent:%d:%d:checkpoint:%d",
			childID,
			link.ActivationSeq,
			*checkpointIteration,
		)

		content, published, err := s.progress.EnqueueChangeFor(ctx, record.RootID, causalID, true)
		s.settleSubagentProgress(log, record.RootID, childID, content, published, err)

		return
	}

	causalID, err = s.subagentStateCausalID(ctx, log, record, link, childID)
	if err != nil {
		return
	}

	content, published, err := s.progress.EnqueueChangeFor(ctx, record.RootID, causalID, true)
	s.settleSubagentProgress(log, record.RootID, childID, content, published, err)
}

// subagentStateCausalID builds the state-transition causal identity. A
// blocking undelivered link publishes the root's whole waiting set instead.
func (s *svc) subagentStateCausalID(
	ctx context.Context,
	log *zap.Logger,
	record *sessionstore.SessionRecord,
	link *subagent.Link,
	childID int64,
) (string, error) {
	blockingRootLink := link.Blocking && link.ParentID == record.RootID
	if !blockingRootLink || link.Terminal() {
		return fmt.Sprintf("subagent:%d:%d:%s", childID, link.ActivationSeq, link.State), nil
	}

	root, err := s.store.GetSession(ctx, record.RootID)
	if err != nil {
		log.Warn("load_subagent_progress_root", zap.Int64("root", record.RootID), zap.Error(err))

		return "", fmt.Errorf("load progress root: %w", err)
	}

	// The waiting slot belongs to the suspension transition (publishWaiting):
	// a capture before suspension commits would freeze a stale card there.
	if root.Status != sessionstore.SessionStatusSuspended {
		return "", errWaitingSlotNotSuspended
	}

	return waitingProgressCausalID(s.collectWaitingProjections(ctx, record.RootID))
}

func (s *svc) settleSubagentProgress(
	log *zap.Logger,
	rootID, childID int64,
	content string,
	published bool,
	err error,
) {
	if errors.Is(err, sessionstore.ErrOutputOwner) || errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return
	}

	if err != nil {
		log.Warn("publish_subagent_progress", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if published {
		s.publish(rootID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: content,
		})
	}
}

func (s *svc) updateLive(ctx context.Context, sessionID int64) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	s.updateLiveLocked(ctx, sessionID)
}

func (s *svc) updateLiveLocked(ctx context.Context, sessionID int64) {
	live := progressruntime.Live{Active: s.HasActiveLoop(sessionID), Working: s.mainModelWorking(ctx, sessionID)}
	live.Context, _ = s.liveContextProjection(ctx, sessionID)
	s.progress.SetLive(sessionID, live)
}

func (s *svc) publishWaiting(
	ctx context.Context,
	sessionID int64,
) {
	projections := s.collectWaitingProjections(ctx, sessionID)
	if len(projections) == 0 {
		return
	}

	waits := make([]sessionevent.WaitItem, len(projections))
	for i, projection := range projections {
		waits[i] = projection.wait
	}

	if err := s.recordWaitingProgress(ctx, sessionID, projections); err != nil {
		logger.Ctx(ctx).Named("daemon.waiting").Warn("record_waiting_progress", zap.Error(err))
	}

	s.publish(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyWaiting, Message: sessionevent.FormatWaiting(waits), Waiting: waits,
	})
}

func (s *svc) collectWaitingProjections(ctx context.Context, sessionID int64) []waitingProjection {
	projections := make([]waitingProjection, 0)

	sleeps, err := s.schedules.PendingSleeps(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.waiting").Warn("list_pending_sleeps", zap.Error(err))
	} else {
		for _, sleep := range sleeps {
			wakeAt := sleep.WakeAt
			projections = append(projections, waitingProjection{
				wait:     sessionevent.WaitItem{Kind: sessionevent.WaitSleep, WakeAt: &wakeAt},
				display:  map[string]any{"wake_at": wakeAt.Format(time.RFC3339)},
				identity: map[string]any{"tool_call_id": sleep.CallID},
			})
		}
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.waiting").Warn("list_subagents", zap.Error(err))
	} else {
		for _, link := range links {
			if link.Blocking && !link.Terminal() && link.State != subagent.StateStopped {
				projections = append(projections, waitingProjection{
					wait:     sessionevent.WaitItem{Kind: sessionevent.WaitSubagent, ChildID: link.ChildID},
					display:  map[string]any{"child_id": link.ChildID},
					identity: map[string]any{"child_id": link.ChildID, "activation_seq": link.ActivationSeq},
				})
			}
		}
	}

	sort.Slice(projections, func(i, j int) bool {
		return waitingIdentityKey(projections[i].identity) < waitingIdentityKey(projections[j].identity)
	})

	return projections
}

// recordWaitingProgress enqueues the durable waiting card for the projected
// set; the canonical replaceable row is its own dedupe, so nothing is returned.
func (s *svc) recordWaitingProgress(
	ctx context.Context,
	sessionID int64,
	projections []waitingProjection,
) error {
	causalID, err := waitingProgressCausalID(projections)
	if err != nil {
		return err
	}

	// A stale waiting card is dropped without a recapture retry: the newer
	// transition that moved the generation owns the next card.
	if _, _, err := s.progress.EnqueueChangeFor(ctx, sessionID, causalID, false); err != nil &&
		!errors.Is(err, sessionstore.ErrProgressSuperseded) && !errors.Is(err, sessionstore.ErrOutputOwner) {
		return fmt.Errorf("enqueue progress: %w", err)
	}

	return nil
}

func waitingProgressCausalID(projections []waitingProjection) (string, error) {
	identities := make([]map[string]any, len(projections))

	for i, projection := range projections {
		identities[i] = projection.identity
	}

	identity, err := canonicalWaitingIdentities(identities)
	if err != nil {
		return "", fmt.Errorf("encode waiting identities: %w", err)
	}

	digest := sha256.Sum256(identity)
	hash := hex.EncodeToString(digest[:])

	return "waiting:" + hash, nil
}

func waitingIdentityKey(identity map[string]any) string {
	if childID, child := positiveWaitingInt(identity["child_id"]); child {
		activation, _ := positiveWaitingInt(identity["activation_seq"])

		return fmt.Sprintf("0:%020d:%020d", childID, activation)
	}

	if callID, ok := identity["tool_call_id"].(string); ok {
		return "1:" + callID
	}

	return "2:invalid"
}

func positiveWaitingInt(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, number > 0
	case int:
		return int64(number), number > 0
	default:
		return 0, false
	}
}

func canonicalWaitingIdentities(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode waiting identities: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil, fmt.Errorf("decode waiting identities: %w", err)
	}

	sort.Slice(items, func(i, j int) bool { return bytes.Compare(items[i], items[j]) < 0 })

	canonical, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("encode canonical waiting identities: %w", err)
	}

	return canonical, nil
}
