package configapply

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
)

type commitOps struct {
	configops.Service
	commit func() configops.Verdict
}

func (o *commitOps) Commit(_ *configops.Staged, _ configops.Pending) configops.Verdict {
	return o.commit()
}

func TestApply_ReleasesClaimOnEveryUnsuccessfulPath(t *testing.T) {
	for _, failure := range []string{"cancelled", "rejected", "panic"} {
		t.Run(failure, func(t *testing.T) {
			commits := 0
			ops := &commitOps{commit: func() configops.Verdict {
				commits++
				if failure == "panic" {
					panic("commit interrupted")
				}
				return configops.Reject("", errors.New("write refused"))
			}}
			service := New(ops)
			staged := &configops.Staged{Summary: "replaced configuration"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			if failure == "panic" {
				require.Panics(t, func() { _, _ = service.apply(ctx, 7, staged) })
			} else {
				result, err := service.apply(ctx, 7, staged)
				require.Error(t, err)
				assert.Nil(t, result)
				if failure == "cancelled" {
					require.ErrorIs(t, err, context.Canceled)
					assert.Zero(t, commits)
				} else {
					assert.Contains(t, err.Error(), "Config change rejected — write refused")
					assert.Contains(t, err.Error(), RefusalReminder)
				}
			}
			service.RestartIfApplied(7)
			select {
			case <-service.Restart():
				t.Fatal("an unsuccessful apply requested restart")
			default:
			}
			ops.commit = configops.OK
			result, err := service.apply(t.Context(), 8, staged)
			require.NoError(t, err, "the unsuccessful apply must release its claim")
			require.NotNil(t, result)
			assert.Contains(t, result.Output, "Config applied")
		})
	}
}
