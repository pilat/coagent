package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

// delegate acts with human-delegated authority: an orchestrator root, or an app when root is nil.
type delegate struct {
	root *sessionstore.SessionRecord
	app  string
}

func (d delegate) started(lineage []int64, target *sessionstore.SessionRecord) bool {
	if d.root != nil {
		return slices.Contains(lineage, controllerapi.SpawnedBy(target.Attributes))
	}

	return controllerapi.SpawnedByApp(target.Attributes) == d.app
}

func (d delegate) actor() orchestrator.Actor {
	if d.root != nil {
		return orchestrator.Actor{Session: d.root.ID}
	}

	return orchestrator.Actor{App: d.app}
}

func (h orchestratorHost) provenance(ctx context.Context, from delegate) (map[string]any, error) {
	actor := from.actor()
	if from.root == nil {
		return map[string]any{
			sessionstore.InputAttributeSenderApp:  actor.App,
			sessionstore.InputAttributeSenderName: "app " + actor.App,
		}, nil
	}

	project, err := h.s.store.GetProjectName(ctx, from.root.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load sender project: %w", err)
	}

	title, _ := from.root.Attributes[controllerapi.SessionAttributeTitle].(string)

	return map[string]any{
		sessionstore.InputAttributeSenderSessionID: actor.Session,
		sessionstore.InputAttributeSenderName:      sessionstore.ChatDisplayName(project, title),
		sessionstore.InputAttributeSenderTitle:     title,
	}, nil
}
