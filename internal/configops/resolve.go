package configops

import (
	"errors"
	"fmt"
)

// Outcome is what a boot that found a pending-apply marker concluded.
type Outcome struct {
	// Pending is the marker that was found, so the caller knows who to tell.
	Pending Pending
	// RolledBack means the config on disk is now the backup again and the caller
	// must re-run its startup validation against it.
	RolledBack bool
	// Verdict is what to deliver into the waiting session.
	Verdict Verdict
}

// ResolvePending decides what a boot makes of a marker. bootErr covers the whole
// startup validation, since enrichment is what pre-write checks cannot see. The
// marker survives: only the caller knows whether the verdict was delivered, and
// a marker cleared before that leaves the waiting session unanswerable.
func (s *svc) ResolvePending(p Pending, bootErr error) (Outcome, error) {
	out := Outcome{Pending: p}

	hash, err := s.ConfigHash()
	if err != nil {
		return out, err
	}

	switch {
	case hash != p.NewHash:
		// The write never landed. Nothing to roll back — the file on disk is
		// still the one that was working.
		out.Verdict = Reject("", errors.New("apply interrupted before it was written — config unchanged"))
	case bootErr == nil:
		out.Verdict = OKWith()
	default:
		if rbErr := s.Rollback(p); rbErr != nil {
			return out, fmt.Errorf("rollback after failed boot %w: %w", bootErr, rbErr)
		}

		out.RolledBack = true
		out.Verdict = Reject(
			"",
			fmt.Errorf("the daemon could not start on the new config, so it was rolled back: %w", bootErr),
		)
	}

	return out, nil
}
