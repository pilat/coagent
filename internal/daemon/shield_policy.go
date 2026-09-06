package daemon

import (
	"context"
	"fmt"
	"sort"
)

//nolint:wsl_v5 // Key-set creation and insertion share one lock.
func (s *svc) recordProcessPolicy(sessionID int64, key string) {
	if key == "" {
		return
	}

	s.processPolicyMu.Lock()
	defer s.processPolicyMu.Unlock()
	if s.processPolicies == nil {
		s.processPolicies = make(map[int64]map[string]struct{})
	}
	keys := s.processPolicies[sessionID]
	if keys == nil {
		keys = make(map[string]struct{})
		s.processPolicies[sessionID] = keys
	}
	keys[key] = struct{}{}
}

//nolint:wsl_v5 // Snapshot and deterministic ordering share one lock.
func (s *svc) recordedProcessPolicies(sessionID int64) []string {
	s.processPolicyMu.Lock()
	defer s.processPolicyMu.Unlock()
	keys := make([]string, 0, len(s.processPolicies[sessionID]))
	for key := range s.processPolicies[sessionID] {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

//nolint:wsl_v5 // Key removal and empty-set cleanup share one lock.
func (s *svc) forgetProcessPolicy(sessionID int64, key string) {
	s.processPolicyMu.Lock()
	defer s.processPolicyMu.Unlock()
	keys := s.processPolicies[sessionID]
	delete(keys, key)
	if len(keys) == 0 {
		delete(s.processPolicies, sessionID)
	}
}

//nolint:wsl_v5 // Every tree member's exact policy is retired before the durable flip.
func (s *svc) retireShieldPolicy(ctx context.Context, rootID int64) error {
	if s.mcpPool == nil {
		return nil
	}

	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list session policies for shield raise: %w", err)
	}

	for _, record := range records {
		if record.ID != rootID && record.RootID != rootID {
			continue
		}

		for _, key := range s.recordedProcessPolicies(record.ID) {
			if err := s.mcpPool.RetirePolicy(key); err != nil {
				return fmt.Errorf("retire old MCP policy for session %d: %w", record.ID, err)
			}
			s.forgetProcessPolicy(record.ID, key)
		}
	}

	return nil
}
