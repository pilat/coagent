package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/ctl"
)

// A dev build on either side makes no claim about being older or newer, so there
// is nothing to offer — and offering an "update" that is a downgrade is worse
// than staying quiet.
func TestSkewed(t *testing.T) {
	tests := []struct {
		name   string
		daemon string
		cli    string
		want   bool
	}{
		{name: "same release", daemon: "v0.4.2", cli: "v0.4.2"},
		{name: "different releases", daemon: "v0.4.1", cli: "v0.4.2", want: true},
		{name: "dev daemon is incomparable", daemon: "dev", cli: "v0.4.2"},
		{name: "dev cli is incomparable", daemon: "v0.4.2", cli: "dev"},
		{name: "both dev", daemon: "dev", cli: "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, skewed(tt.daemon, tt.cli))
		})
	}
}

// The driver list is the fixed set the config loader accepts. A driver the
// prompt offers but the loader rejects would produce a config the daemon cannot
// start on — which is exactly the state onboarding exists to prevent.
func TestDriverPrompt_MatchesTheSchema(t *testing.T) {
	assert.Equal(t, []string{"anthropic", "openrouter", "openai", "google-sa"}, driverPrompt)
}

func TestManagerLine(t *testing.T) {
	tests := []struct {
		name string
		in   ctl.ManagerStatus
		want string
	}{
		{
			name: "running",
			in:   ctl.ManagerStatus{ID: "tg", Driver: "telegram", Enabled: true, Running: true},
			want: "manager tg (telegram) · running",
		},
		{
			name: "enabled but down, with the reason",
			in:   ctl.ManagerStatus{ID: "tg", Driver: "telegram", Enabled: true, Error: "401 unauthorized"},
			want: "manager tg (telegram) · not running · 401 unauthorized",
		},
		{
			name: "disabled",
			in:   ctl.ManagerStatus{ID: "tg", Driver: "telegram"},
			want: "manager tg (telegram) · disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, managerLine(tt.in))
		})
	}
}

// Declining the update must keep what the daemon already reported. Forgetting it
// would send a configured machine back through first-provider setup.
func TestOfferUpdate_DeclineKeepsTheStatusAlreadyRead(t *testing.T) {
	answerStdin(t, "n\n")

	current := ctl.StatusResult{
		ConfigPresent: true,
		Providers:     []ctl.ProviderStatus{{Name: "work", Driver: "anthropic"}},
	}

	got, code := offerUpdate(context.Background(), "", "v0.4.1", current)

	assert.Equal(t, exitOK, code)
	assert.Equal(t, current, got, "the caller still sees the provider it has")
}

// Only "this daemon never heard of the op" earns the sudo fallback — anything
// else is a working daemon that failed, which an install does not fix.
func TestUnknownMethod(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "a daemon a version behind",
			err:  fmt.Errorf("restart_daemon: %w", &ctl.Error{Code: ctl.CodeMethodNotFound, Message: "unknown method"}),
			want: true,
		},
		{
			name: "a daemon that knows the op and failed",
			err:  fmt.Errorf("restart_daemon: %w", &ctl.Error{Code: ctl.CodeInternal, Message: "boom"}),
		},
		{
			name: "a transport failure",
			err:  errors.New("write request: broken pipe"),
		},
		{name: "no error at all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, unknownMethod(tt.err))
		})
	}
}

// answerStdin feeds the prompts a scripted reply for the length of one test.
func answerStdin(t *testing.T, reply string) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	_, err = w.WriteString(reply)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	original := os.Stdin
	os.Stdin = r

	t.Cleanup(func() {
		os.Stdin = original

		_ = r.Close()
	})
}
