package sandboxnet

import (
	"errors"
	"fmt"
	"strconv"
)

// ParseSetupArgs reads the setup child's own flags.
func ParseSetupArgs(args []string) (SetupConfig, error) {
	config := SetupConfig{ReturnFD: -1}

	for index := 0; index < len(args); index += 2 {
		if args[index] != "--return-fd" {
			return SetupConfig{}, fmt.Errorf("unknown setup flag %q", args[index])
		}

		if index+1 >= len(args) {
			return SetupConfig{}, errors.New("setup flag \"--return-fd\" needs a value")
		}

		fd, err := strconv.Atoi(args[index+1])
		if err != nil {
			return SetupConfig{}, fmt.Errorf("parse return fd %q: %w", args[index+1], err)
		}

		config.ReturnFD = fd
	}

	if config.ReturnFD < 0 {
		return SetupConfig{}, errors.New("setup requires --return-fd")
	}

	return config, nil
}
