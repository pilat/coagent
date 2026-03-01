package shellenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const captureTimeout = 10 * time.Second

// dumpMarker separates rc startup noise (motd/greeters print to stdout) from the
// dump; everything before it is discarded.
const dumpMarker = "__COAGENT_SHELLENV_SNAPSHOT_9f2a__"

// exportMarker delimits the `export -p` section so the readonly filter touches
// only exported-var lines — never function/heredoc bodies from `declare -f`.
const exportMarker = "__COAGENT_SHELLENV_EXPORTS_9f2a__"

// dumpCommand ordering is load-bearing: `shopt -p` before `declare -f` (extglob
// must be re-enabled before bodies parse); `export -p` last, behind its marker.
const dumpCommand = "printf '\\n%s\\n' '" + dumpMarker + "'; shopt -p; declare -f; alias -p; " +
	"printf '\\n%s\\n' '" + exportMarker + "'; export -p"

// capture runs the user's login+interactive bash in workDir and returns a
// re-sourceable snapshot. `-l -i` is required: plain `-l` shadows mise activation.
func (p *provider) capture(ctx context.Context, workDir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	// SECURITY: this shell gets os.Environ() only — never a secrets map. coagent
	// secrets live solely in the in-memory Secrets map the config package loads.
	cmd := exec.CommandContext(ctx, p.shell, "-l", "-i", "-c", dumpCommand)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	// An rc-spawned lingering daemon (ssh-agent/direnv) can hold the stdout pipe
	// open; WaitDelay bounds Run() so it can't hang forever under the per-key lock.
	cmd.WaitDelay = captureTimeout

	// Stdin/Stderr left nil → /dev/null: stops the interactive shell blocking on
	// read/prompt, and discards its job-control noise.
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("capture shell snapshot: %w", err)
	}

	return parseDump(out.Bytes())
}

// parseDump strips rc startup noise before the dump marker, then filters readonly
// exports out of the `export -p` section only (leaving functions/aliases intact).
func parseDump(raw []byte) ([]byte, error) {
	_, body, found := bytes.Cut(raw, []byte(dumpMarker+"\n"))
	if !found {
		return nil, errors.New("capture shell snapshot: dump marker not found")
	}

	prefix, exports, found := bytes.Cut(body, []byte(exportMarker+"\n"))
	if !found {
		return nil, errors.New("capture shell snapshot: export marker not found")
	}

	return append(prefix, filterReadonly(exports)...), nil
}

// filterReadonly drops top-level `declare -<flags>` lines with both r and x set:
// re-declaring a readonly export on replay warns, and that warning reaches the model.
func filterReadonly(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	kept := lines[:0]

	for _, line := range lines {
		if isReadonlyExport(line) {
			continue
		}

		kept = append(kept, line)
	}

	return bytes.Join(kept, []byte("\n"))
}

func isReadonlyExport(line []byte) bool {
	const prefix = "declare -"

	if !bytes.HasPrefix(line, []byte(prefix)) {
		return false
	}

	flags := line[len(prefix):]
	if i := bytes.IndexByte(flags, ' '); i >= 0 {
		flags = flags[:i]
	}

	return bytes.IndexByte(flags, 'r') >= 0 && bytes.IndexByte(flags, 'x') >= 0
}
