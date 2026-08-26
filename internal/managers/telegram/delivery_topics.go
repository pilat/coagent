package telegram

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func (t *outputTransport) replaceSession(
	ctx context.Context,
	claim *controllerapi.OutputClaimData,
) managerdelivery.Result {
	oldID, ok := positiveID(claim.Attributes["old_session_id"])
	if !ok {
		return managerdelivery.Result{Error: "invalid session replaced output"}
	}

	topicID, patch, err := t.ensureSessionTopic(ctx, claim)
	if err != nil {
		return t.deliveryFailure(err)
	}

	workDir, _ := claim.Attributes["work_dir"].(string)
	t.manager.remapTopic(oldID, claim.SessionID, topicID, workDir)

	return managerdelivery.Result{SessionPatch: patch}
}

func positiveID(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number <= 0 || number != float64(int64(number)) {
		return 0, false
	}

	return int64(number), true
}

func (t *outputTransport) openSession(
	ctx context.Context,
	claim *controllerapi.OutputClaimData,
) managerdelivery.Result {
	name, _ := claim.Attributes["name"].(string)

	workDir, _ := claim.Attributes["work_dir"].(string)
	if name == "" || workDir == "" {
		return managerdelivery.Result{Error: "invalid session opened output"}
	}

	_, patch, err := t.ensureSessionTopic(ctx, claim)
	if err != nil {
		return t.deliveryFailure(err)
	}

	return managerdelivery.Result{SessionPatch: patch}
}

func (t *outputTransport) closeSession(
	ctx context.Context,
	claim *controllerapi.OutputClaimData,
) managerdelivery.Result {
	topicID, ok := topicIDFromAttributes(claim.SessionAttributes)
	if !ok {
		topicID, ok = t.manager.getTopicBySessionID(claim.SessionID)
	}

	if !ok {
		return managerdelivery.Result{}
	}

	if err := t.manager.deleteForumTopic(ctx, topicID); err != nil {
		if !isTopicMissing(err) {
			return t.deliveryFailure(err)
		}
	}

	t.manager.unregisterTopic(claim.SessionID)
	t.manager.deleteWorkDir(claim.SessionID)

	return managerdelivery.Result{}
}

func (t *outputTransport) ensureSessionTopic(
	ctx context.Context,
	claim *controllerapi.OutputClaimData,
) (int64, map[string]any, error) {
	if topicID, ok := t.manager.getTopicBySessionID(claim.SessionID); ok {
		exists, err := t.manager.forumTopicExists(ctx, topicID)
		if err != nil {
			return 0, nil, fmt.Errorf("verify cached session topic: %w", err)
		}

		if exists {
			return topicID, topicPatch(claim.SessionAttributes, topicID), nil
		}

		t.manager.unregisterTopic(claim.SessionID)
	}

	if topicID, ok := topicIDFromAttributes(claim.SessionAttributes); ok {
		exists, err := t.manager.forumTopicExists(ctx, topicID)
		if err != nil {
			return 0, nil, fmt.Errorf("verify session topic: %w", err)
		}

		if exists {
			t.manager.registerTopic(claim.SessionID, topicID)
			return topicID, nil, nil
		}
	}

	name, _ := claim.Attributes["name"].(string)

	workDir, _ := claim.Attributes["work_dir"].(string)
	if name == "" {
		name = fmt.Sprintf("Session %d", claim.SessionID)
	}

	topicID, err := t.manager.createForumTopic(ctx, name, t.manager.cfg.SessionTopicIconEmojiID)
	if err != nil {
		return 0, nil, fmt.Errorf("create session topic: %w", err)
	}

	t.manager.registerTopic(claim.SessionID, topicID)

	if workDir != "" {
		t.manager.setWorkDir(claim.SessionID, workDir)
	}

	return topicID, map[string]any{"telegram_topic_id": topicID}, nil
}

func topicPatch(attrs map[string]any, topicID int64) map[string]any {
	current, ok := topicIDFromAttributes(attrs)
	if ok && current == topicID {
		return nil
	}

	return map[string]any{"telegram_topic_id": topicID}
}
