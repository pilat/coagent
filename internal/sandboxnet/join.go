package sandboxnet

import (
	"errors"
	"fmt"
	"strconv"
)

// JoinCommand is the hidden subcommand that joins a session tree's network
// namespace and then execs the command.
const JoinCommand = "__coagent_net_join"

// joinArgSeparator ends the join flags and starts the command to exec.
const joinArgSeparator = "--"

// JoinConfig holds the inherited namespace descriptor the trusted entry uses.
type JoinConfig struct{ NetNSFD int }

// ParseJoinArgs reads the join child's own flags. Everything after the
// separator is the command to exec, passed verbatim.
func ParseJoinArgs(args []string) (JoinConfig, []string, error) {
	cfg := JoinConfig{}

	index := 0

	for index < len(args) {
		arg := args[index]

		if arg == joinArgSeparator {
			argv := append([]string(nil), args[index+1:]...)
			if len(argv) == 0 {
				return JoinConfig{}, nil, errors.New("network join requires a command after " + joinArgSeparator)
			}

			if cfg.NetNSFD < 3 {
				return JoinConfig{}, nil, errors.New("network join requires --netns-fd")
			}

			return cfg, argv, nil
		}

		value, next, err := joinFlagValue(args, index)
		if err != nil {
			return JoinConfig{}, nil, err
		}

		index = next

		switch arg {
		case "--netns-fd":
			fd, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				return JoinConfig{}, nil, fmt.Errorf("parse network namespace descriptor: %w", parseErr)
			}

			cfg.NetNSFD = fd
		default:
			return JoinConfig{}, nil, fmt.Errorf("unknown join flag %q", arg)
		}
	}

	return JoinConfig{}, nil, errors.New("network join requires a command after " + joinArgSeparator)
}

// RunJoinMode is the hidden join entry point. It reports whether it handled the
// invocation, so the binary can continue to its ordinary dispatch.
func RunJoinMode(args []string) (bool, error) {
	if len(args) == 0 || args[0] != JoinCommand {
		return false, nil
	}

	cfg, argv, err := ParseJoinArgs(args[1:])
	if err != nil {
		return true, err
	}

	return true, RunJoin(cfg, argv)
}

func joinFlagValue(args []string, index int) (string, int, error) {
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("join flag %q needs a value", args[index])
	}

	return args[index+1], index + 2, nil
}
