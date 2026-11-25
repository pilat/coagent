package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type Process struct {
	Argv  []string
	Env   []string
	Stdin []byte
}

type Outcome struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Launcher func(context.Context, Process) (Outcome, error)

func (h *Hooks) run(ctx context.Context, event string, names []string, payload stdinPayload) []decision {
	data, err := json.Marshal(payload)
	if err != nil {
		return h.invalidPayload(ctx, event, names, err)
	}

	results := make([]decision, len(h.handlers[event]))
	var wg sync.WaitGroup

	for index, entry := range h.handlers[event] {
		if !entry.match.matches(names) {
			continue
		}

		wg.Go(func() { results[index] = h.runHandler(ctx, event, entry, data) })
	}

	wg.Wait()

	return results
}

func (h *Hooks) runHandler(ctx context.Context, event string, entry handler, data []byte) decision {
	timeout := 600 * time.Second
	if entry.timeout > 0 {
		timeout = time.Duration(entry.timeout * float64(time.Second))
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := []string{"bash", "-c", entry.command}
	if entry.execForm {
		argv = append([]string{entry.command}, entry.args...)
		for index := range argv {
			argv[index] = strings.ReplaceAll(argv[index], "${CLAUDE_PROJECT_DIR}", h.workDir)
		}
	}

	outcome, err := h.launch(ctx, Process{
		Argv:  argv,
		Env:   []string{"CLAUDE_PROJECT_DIR=" + h.workDir},
		Stdin: data,
	})

	if entry.async {
		return decision{}
	}

	return interpret(ctx, event, entry, outcome, err)
}

func (h *Hooks) invalidPayload(ctx context.Context, event string, names []string, err error) []decision {
	results := make([]decision, len(h.handlers[event]))
	for index, entry := range h.handlers[event] {
		if !entry.match.matches(names) {
			continue
		}

		if entry.async {
			continue
		}

		results[index] = failureDecision(ctx, event, entry, Outcome{}, err)
	}

	return results
}
