package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/install"
)

// TestSudoCommand pins the re-exec: the absolute path captured at boot, the same
// verb, and the caller's stdio so sudo can prompt on the terminal already here.
func TestSudoCommand(t *testing.T) {
	cmd := sudoCommand(context.Background(), install.ActionInstall)

	assert.Equal(t, []string{"sudo", selfExecPath, "daemon", "install"}, cmd.Args)
	assert.Equal(t, os.Stdin, cmd.Stdin)
	assert.Equal(t, os.Stdout, cmd.Stdout)
	assert.Equal(t, os.Stderr, cmd.Stderr)
}

// TestShouldEscalate is the gate itself: every lifecycle verb writes to /etc or
// the system launchd domain, and only a non-root caller has to go get that.
func TestShouldEscalate(t *testing.T) {
	tests := []struct {
		name   string
		action string
		euid   int
		want   bool
	}{
		{name: "install as user", action: install.ActionInstall, euid: 1000, want: true},
		{name: "uninstall as user", action: install.ActionUninstall, euid: 1000, want: true},
		{name: "start as user", action: install.ActionStart, euid: 1000, want: true},
		{name: "stop as user", action: install.ActionStop, euid: 1000, want: true},
		{name: "restart as user", action: install.ActionRestart, euid: 1000, want: true},
		{name: "install already under sudo", action: install.ActionInstall, euid: 0},
		{name: "restart already under sudo", action: install.ActionRestart, euid: 0},
		{name: "an unknown verb never escalates", action: "frobnicate", euid: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := geteuid
			geteuid = func() int { return tt.euid }

			t.Cleanup(func() { geteuid = original })

			assert.Equal(t, tt.want, shouldEscalate(tt.action))
		})
	}
}
