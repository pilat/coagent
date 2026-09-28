package daemon

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type managerRoutes interface {
	Source() sessionbus.Source
	Publish(int64, sessionevent.Notification)
	SetAttributes(context.Context, int64, map[string]any) error
	ResolveReplacement(context.Context, int64) (int64, error)
	Replace(context.Context, int64, int64, func(context.Context, int64) error) (int64, error)
}

var _ managerRoutes = (*managerRouteSet)(nil)

type sessionRouteStore interface {
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	SetAttributes(context.Context, int64, map[string]any) error
	CreateReplacementSession(context.Context, int64) (*sessionstore.SessionRecord, error)
}

type managerReplacements interface {
	ReplaceManagerRoot(
		context.Context, int64, string, string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ReplaceManagerRootForInput(
		context.Context, int64, int64, string, string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ResolveReplacement(context.Context, int64, string) (int64, error)
}

type routeProjects interface {
	GetProjectWorkDir(context.Context, int64) (string, error)
	GetProjectName(context.Context, int64) (string, error)
}

// Tree fences precede ownershipMu, then cacheMu. Publication takes only cacheMu
// because retirement publishes while ownershipMu is held.
type managerRouteSet struct {
	sessionStore sessionRouteStore
	managerRoots managerReplacements
	store        routeProjects
	pubsub       sessionbus.Bus
	ownershipMu  sync.Mutex
	cacheMu      sync.Mutex
	childCache   map[int64]bool
	ownerCache   map[int64]string
}

func newManagerRoutes(
	sessions sessionRouteStore,
	replacements managerReplacements,
	projects routeProjects,
	bus sessionbus.Bus,
) managerRoutes {
	return &managerRouteSet{
		sessionStore: sessions, managerRoots: replacements, store: projects, pubsub: bus,
		childCache: make(map[int64]bool), ownerCache: make(map[int64]string),
	}
}

// Source exposes subscriptions without granting publication authority.
func (s *managerRouteSet) Source() sessionbus.Source {
	return s.pubsub
}

// ResolveReplacement preserves the original owner while following durable replacements.
func (s *managerRouteSet) ResolveReplacement(ctx context.Context, sessionID int64) (int64, error) {
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("load session for replacement resolution: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	resolved, err := s.managerRoots.ResolveReplacement(ctx, sessionID, owner)
	if err != nil {
		return 0, fmt.Errorf("resolve replacement session: %w", err)
	}

	return resolved, nil
}

// SetAttributes preserves committed ownership and serializes claims with replacement.
func (s *managerRouteSet) SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error {
	s.ownershipMu.Lock()
	defer s.ownershipMu.Unlock()

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session before setting attributes: %w", err)
	}

	if rec == nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	attrs = maps.Clone(attrs)
	existingOwner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	requestedOwner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	claimingOwner := existingOwner == "" && requestedOwner != ""

	if claimingOwner && (rec.Status == sessionstore.SessionStatusTerminating || rec.KilledAt != nil) {
		return fmt.Errorf("session %d is closing and cannot acquire a manager owner", sessionID)
	}

	if existingOwner != "" && requestedOwner != "" && existingOwner != requestedOwner {
		return fmt.Errorf("session %d belongs to manager %q", sessionID, existingOwner)
	}

	if existingOwner != "" {
		if attrs == nil {
			attrs = make(map[string]any)
		}

		attrs[controllerapi.SessionAttributeManagerID] = existingOwner
		requestedOwner = existingOwner
	}

	if err := s.sessionStore.SetAttributes(ctx, sessionID, attrs); err != nil {
		return fmt.Errorf("set session attributes: %w", err)
	}

	s.cacheMu.Lock()
	s.ownerCache[sessionID] = requestedOwner
	s.cacheMu.Unlock()

	return nil
}

// Replace requires the caller's tree fence and retires the old root before releasing ownership.
func (s *managerRouteSet) Replace(
	ctx context.Context,
	sessionID, inputID int64,
	retire func(context.Context, int64) error,
) (int64, error) {
	log := logger.Ctx(ctx).Named("manager.clear")

	s.ownershipMu.Lock()
	defer s.ownershipMu.Unlock()

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return 0, fmt.Errorf("session %d is already killed", sessionID)
	}

	workDir, _ := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	projectName, _ := s.store.GetProjectName(ctx, rec.ProjectID)
	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	var newRec *sessionstore.SessionRecord

	//nolint:nestif // Owner-aware replacement is the one boundary that preserves a manager surface.
	if owner != "" {
		if inputID > 0 {
			newRec, _, err = s.managerRoots.ReplaceManagerRootForInput(ctx, sessionID, inputID, projectName, workDir)
		} else {
			newRec, _, err = s.managerRoots.ReplaceManagerRoot(ctx, sessionID, projectName, workDir)
		}

		if err != nil {
			return 0, fmt.Errorf("replace manager session: %w", err)
		}
	} else {
		newRec, err = s.sessionStore.CreateReplacementSession(ctx, sessionID)
		if err != nil {
			return 0, fmt.Errorf("create replacement session: %w", err)
		}
	}

	name := fmt.Sprintf("%s - %d", projectName, newRec.ID)
	s.Publish(sessionID, sessionevent.Notification{
		Type:         sessionevent.NotifySessionCleared,
		OldSessionID: sessionID,
		NewSessionID: newRec.ID,
		Name:         name,
		WorkDir:      workDir,
		Attributes:   rec.Attributes,
	})

	if err := retire(ctx, sessionID); err != nil {
		log.Warn("clear_kill_old_session_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}

	return newRec.ID, nil
}
