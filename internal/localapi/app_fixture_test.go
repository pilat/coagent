package localapi

import (
	"context"
	"fmt"
)

func (b *apiBackend) SpawnForApp(ctx context.Context, _ string, workDir, worktree, model string) (int64, error) {
	project, err := b.store.GetOrCreateProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("create app test project: %w", err)
	}
	return b.CreateSession(ctx, project, worktree, "", model, nil)
}

func (b *apiBackend) SendForApp(ctx context.Context, _ string, id int64, text, key string) (int64, error) {
	return b.SendToSessionResolved(ctx, id, text, key)
}

func (b *apiBackend) StopForApp(ctx context.Context, _ string, id int64) (int64, error) {
	return b.SendToSessionResolved(ctx, id, "/stop", "")
}

func (b *apiBackend) KillForApp(ctx context.Context, _ string, id int64) (int64, error) {
	return b.SendToSessionResolved(ctx, id, "/kill", "")
}

func (*apiBackend) TurnOver(_ context.Context, id int64) (int64, bool, error) {
	return id, true, nil
}
