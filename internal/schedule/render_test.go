package schedule_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
)

func TestRender_PreservesLiteralCronValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  string
		quoted string
	}{
		{name: "plain", value: "0 9 * * *", quoted: "`0 9 * * *`"},
		{name: "backtick", value: "a`b", quoted: "`a`b`"},
		{name: "newline", value: "0\n9 * * *", quoted: "`0\n9 * * *`"},
		{name: "controls", value: "a\r\t\x00b", quoted: "`a\r\t\x00b`"},
		{name: "invalid UTF-8", value: "a\xffb", quoted: "`a\xffb`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store, projects, sessions := newTestDB(t)
			projectID := testProject(t, projects, "/tmp/project")
			session, err := sessions.CreateSession(ctx, projectID, "", "", nil)
			require.NoError(t, err)
			svc := schedule.NewService(store, sessions)
			created, err := svc.AddRecurring(ctx, session.ID, tt.value, "check", false)
			require.NoError(t, err)

			rendered, err := svc.Render(ctx, session.ID)
			require.NoError(t, err)
			want := "## Schedules (1)\n" +
				fmt.Sprintf("- #%d · cron %s (UTC) · check\n", created.ID, tt.quoted) +
				"\nAsk me in chat to add, change, or remove a schedule."
			assert.Equal(t, want, rendered)
		})
	}
}
