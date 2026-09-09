package builtin

import (
	"fmt"
	"os"
	"testing"

	"github.com/pilat/coagent/internal/backgroundprocess"
)

func TestMain(m *testing.M) {
	if handled, err := backgroundprocess.RunGuardian(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	os.Exit(m.Run())
}
