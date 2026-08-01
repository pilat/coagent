package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

// chownFn and geteuid are the seams the ownership tests replace: neither the
// call nor the root branch is observable in a normal test process.
var (
	chownFn = os.Chown
	geteuid = os.Geteuid
)

// installBinary copies the running executable to dst through a temp file in the
// same directory. The rename is what makes reinstall-over-running work: the live
// process keeps its old inode, so the copy never hits ETXTBSY.
func installBinary(dst string, t target) error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}

	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}

	created, err := mkdirAllTracked(filepath.Dir(dst))
	if err != nil {
		return err
	}

	// Handed over before the copy, not after: a failure past this point must not
	// leave a root-owned directory that a retry would then skip.
	if err := giveToTarget(t, created...); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), coagenthome.DirName+"-*")
	if err != nil {
		return fmt.Errorf("create temp file next to %s: %w", dst, err)
	}

	tmpName := tmp.Name()

	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("copy binary: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}

	if err := os.Chmod(tmpName, binaryMode); err != nil {
		return fmt.Errorf("chmod temp binary: %w", err)
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("install binary to %s: %w", dst, err)
	}

	return giveToTarget(t, dst)
}

// mkdirAllTracked creates dir and reports the segments it had to create, so a
// root install hands over exactly those. os.MkdirAll cannot answer that.
func mkdirAllTracked(dir string) ([]string, error) {
	var missing []string

	for p := dir; !exists(p); p = filepath.Dir(p) {
		missing = append(missing, p)

		if parent := filepath.Dir(p); parent == p {
			break
		}
	}

	slices.Reverse(missing)

	for _, p := range missing {
		if err := os.Mkdir(p, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", p, err)
		}
	}

	return missing, nil
}

// giveToTarget hands root-written paths under the target's home to that account,
// or the sudo-free update cannot replace what the install wrote.
func giveToTarget(t target, paths ...string) error {
	if geteuid() != 0 {
		return nil
	}

	for _, p := range paths {
		if err := chownFn(p, t.uid, t.gid); err != nil {
			return fmt.Errorf("chown %s to %s: %w", p, t.name, err)
		}
	}

	return nil
}

// writeFileAtomic replaces path in one rename, so a service manager reading the
// directory never sees a half-written unit.
func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), coagenthome.DirName+"-*")
	if err != nil {
		return fmt.Errorf("create temp file next to %s: %w", path, err)
	}

	tmpName := tmp.Name()

	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}

	if err := os.Chmod(tmpName, unitMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// run executes a service-manager command and folds its output into the error.
// The output is the whole diagnostic value of systemctl and launchctl failures.
func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}

		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
	}

	return nil
}

// succeeds reports whether a query command exited zero, which is how both
// service managers answer "is this thing running".
func succeeds(ctx context.Context, name string, args ...string) bool {
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
