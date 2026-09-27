package configops

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "coagent-configops-test-")
	if err != nil {
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
