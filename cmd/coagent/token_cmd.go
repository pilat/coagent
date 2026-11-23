package main

import (
	"fmt"
	"io"
	"os"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/localapi"
)

func runToken(args []string, out io.Writer) int {
	if len(args) != 1 || !config.ValidAPIClientID(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: coagent token <client-id>\nclient-id must match ^[a-z0-9][a-z0-9_-]{0,31}$")
		return exitError
	}

	token, err := localapi.NewToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}

	_, err = fmt.Fprintf(out, "Token for API client %q (shown once; give it to the client):\n%s\n\n"+
		"Add to ~/.coagent/config.yaml, then restart the daemon:\n"+
		"api:\n  clients:\n    - id: %s\n      role: operator\n      token_sha256: %s\n",
		args[0], token, args[0], localapi.TokenHash(token))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}

	return exitOK
}
