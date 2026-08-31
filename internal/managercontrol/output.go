package managercontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *service) bindOutputDelivery(
	ctx context.Context,
	managerID string,
	data controllerapi.OutputBindingData,
) error {
	if err := requireManagerIdentity(managerID); err != nil {
		return err
	}

	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := s.outputs.BindManager(ctx, managerID, data.Driver, data.Attributes); err != nil {
		return fmt.Errorf("bind output delivery: %w", err)
	}

	if _, err := s.outputs.RetryBlockedHead(ctx, managerID); err != nil {
		return fmt.Errorf("retry blocked output head: %w", err)
	}

	return nil
}

func (s *service) claimOutput(
	ctx context.Context,
	managerID string,
) (*controllerapi.OutputClaimData, error) {
	if s.outputs == nil {
		return nil, errors.New("output delivery is unavailable")
	}

	claim, err := s.outputs.ClaimOutputHead(ctx, managerID)

	var pending *sessionstore.OutputRetryPendingError
	if errors.As(err, &pending) {
		return nil, &controllerapi.OutputRetryPendingError{NextAt: pending.NextAt}
	}

	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil, controllerapi.ErrNoOutput
	}

	if err != nil {
		return nil, fmt.Errorf("claim output head: %w", err)
	}

	return outputClaimData(claim), nil
}

func outputClaimData(claim *sessionstore.OutputClaim) *controllerapi.OutputClaimData {
	data := &controllerapi.OutputClaimData{
		ID: claim.Output.ID, SessionID: claim.Output.SessionID,
		Type: string(claim.Output.Type), Content: claim.Output.Content,
		Attributes: claim.Output.Attributes, AttemptID: claim.Output.AttemptID,
		AttemptSeq: claim.Output.AttemptSeq, SourceKey: claim.Output.SourceKey,
		SessionAttributes: claim.SessionAttributes, ReleasesInput: claim.Output.ReleasesInput,
	}
	if claim.PreviousDeliveredOutput != nil {
		data.PreviousMessageAttributes = claim.PreviousDeliveredOutput.Attributes
		data.PreviousMessageType = string(claim.PreviousDeliveredOutput.Type)
		data.PreviousModelInputGeneration = generationFromAttributes(claim.PreviousDeliveredOutput.Attributes)
	}

	data.ModelInputGeneration = generationFromAttributes(claim.Output.Attributes)

	return data
}

func generationFromAttributes(attributes map[string]any) *int64 {
	value, ok := attributes[sessionstore.ModelInputGenerationAttribute].(float64)
	if !ok {
		return nil
	}

	generation := int64(value)

	return &generation
}

func (s *service) ackOutput(
	ctx context.Context,
	managerID string,
	data controllerapi.OutputAckData,
) error {
	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := s.outputs.AckOutput(
		ctx, managerID, data.ID, data.AttemptID, data.MessageIDs, data.SessionPatch,
	); err != nil {
		return fmt.Errorf("ack output: %w", err)
	}

	if err := s.backend.ReconcileOutputReadiness(ctx, data.ID); err != nil {
		return fmt.Errorf("reconcile output readiness: %w", err)
	}

	return nil
}

func (s *service) retryOutput(
	ctx context.Context,
	managerID string,
	data controllerapi.OutputRetryData,
) error {
	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := s.outputs.RetryOutput(ctx, managerID, data.ID, data.AttemptID, data.Error, data.NextAt); err != nil {
		return fmt.Errorf("retry output: %w", err)
	}

	return nil
}

func (s *service) blockOutput(
	ctx context.Context,
	managerID string,
	data controllerapi.OutputBlockData,
) error {
	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := s.outputs.BlockOutput(ctx, managerID, data.ID, data.AttemptID, data.Error); err != nil {
		return fmt.Errorf("block output: %w", err)
	}

	return nil
}

func (s *service) wakeOutput(ctx context.Context, managerID string) error {
	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if _, err := s.outputs.WakeOutputHead(ctx, managerID); err != nil {
		return fmt.Errorf("wake output head: %w", err)
	}

	return nil
}

func (s *service) repairSessionSurface(
	ctx context.Context,
	managerID string,
	sessionID int64,
	binding string,
) error {
	if err := s.requireOwnedSession(ctx, managerID, sessionID); err != nil {
		return err
	}

	if s.outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	record, err := s.backend.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for surface repair: %w", err)
	}

	name, err := s.backend.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load repair project name: %w", err)
	}

	workDir, err := s.backend.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load repair work dir: %w", err)
	}

	lifecycleID, err := s.outputs.LatestLifecycleOutputID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load surface repair lifecycle: %w", err)
	}

	digest := sha256.Sum256([]byte(binding))
	attributes := map[string]any{"name": name, "work_dir": workDir}
	key := "session:" + strconv.FormatInt(sessionID, 10) + ":repair:" +
		strconv.FormatInt(lifecycleID, 10) + ":" + hex.EncodeToString(digest[:])

	_, err = s.outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: sessionID, Type: sessionstore.OutputSessionOpened,
		Attributes: attributes, SourceKey: key,
		Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputSessionOpened, "", sessionID, attributes),
	})
	if err != nil {
		return fmt.Errorf("enqueue session surface repair: %w", err)
	}

	return nil
}

func (s *service) outputQueueStatus(
	ctx context.Context,
	managerID string,
) (controllerapi.OutputQueueStatusData, error) {
	if s.outputs == nil {
		return controllerapi.OutputQueueStatusData{}, errors.New("output delivery is unavailable")
	}

	status, err := s.outputs.OutputQueueStatus(ctx, managerID)
	if err != nil {
		return controllerapi.OutputQueueStatusData{}, fmt.Errorf("load output queue status: %w", err)
	}

	data := controllerapi.OutputQueueStatusData{
		Pending: status.Pending, BlockedID: status.BlockedID, DeliveryError: status.DeliveryError,
	}
	if status.BlockedAt != nil {
		data.BlockedForSec = int64(time.Since(*status.BlockedAt).Seconds())
	}

	return data, nil
}

func (s *service) unresolvedOutputOwners(ctx context.Context) ([]string, error) {
	if s.outputs == nil {
		return nil, errors.New("output delivery is unavailable")
	}

	values, err := s.outputs.ListUnresolvedOutputOwners(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unresolved output owners: %w", err)
	}

	return values, nil
}
