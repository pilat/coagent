package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

// Lifecycle verbs. The CLI, the unit file and the sudo handoff agree
// on these names, so they are spelled once.
const (
	ActionInstall   = "install"
	ActionUninstall = "uninstall"
	ActionStart     = "start"
	ActionStop      = "stop"
	ActionRestart   = "restart"
)

const (
	// binaryMode is what the installed binary gets: readable and executable by
	// everyone, writable only by the owner that put it there.
	binaryMode = 0o755
	// unitMode is what the unit gets.
	unitMode = 0o644
)

// Platform names, used where the answer travels to a UI rather than to a switch.
const platformLinux = "linux"

// scopeSystem is the only scope there is: a system service that drops to the
// target user.
const scopeSystem = "system"

var errUnsupported = errors.New("service installation is supported on linux (systemd) only")

// platformSupported reports whether this host runs the supported runtime. It is
// the first check in every exported entry so direct package callers cannot
// write a binary or resolve a unit on an unsupported host merely because the
// CLI guard was bypassed.
func platformSupported(goos string) bool { return goos == platformLinux }

// unsupportedPlatformError names the offending platform so a refusal from a
// manually cross-built binary stays diagnosable. It wraps the sentinel so
// errors.Is keeps working for callers.
func unsupportedPlatformError(goos string) error {
	return fmt.Errorf("%w (GOOS=%q)", errUnsupported, goos)
}

// Info is what a UI can learn about the service without talking to the daemon:
// unit presence, paths, and whether the service manager considers it active.
type Info struct {
	Platform   string
	Supported  bool
	Installed  bool
	Active     bool
	Scope      string
	UnitName   string
	UnitPath   string
	BinaryPath string
	RunAsUser  string
	LogCommand string
}

// Plan is the install pre-flight: what gets created, where, and under which
// account — the facts to show before asking for a password.
type Plan struct {
	Description string
	UnitPath    string
	BinaryPath  string
	RunAsUser   string
	Command     string
	NeedsRoot   bool
}

// Manager performs service lifecycle actions for one platform and scope.
type Manager interface {
	// Info reports the local, socket-free view of the service.
	Info() Info
	// Plan describes what Install would do.
	Plan() Plan
	// Install copies the binary, writes the unit, enables and starts it. It is
	// idempotent: reinstalling over a running service replaces both and restarts.
	Install(ctx context.Context) error
	// Uninstall stops the service, then removes the unit and the binary. It
	// leaves ~/.coagent alone — config and secrets outlive the service.
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
}

// target is the account the service runs as. Under sudo that is the invoking
// user, never root: the daemon owns ~/.coagent, and root's copy of it is not the
// one anybody configured.
type target struct {
	name string
	home string
	uid  int
	gid  int
}

// New returns the service manager for this platform. There is one install mode:
// a systemd system unit that drops to the target user.
func New() (Manager, error) {
	if !platformSupported(runtime.GOOS) {
		return nil, unsupportedPlatformError(runtime.GOOS)
	}

	return newSystemd()
}

// UpdateBinary replaces the installed binary with the running one. It needs no
// privileges — that is the whole point of keeping the binary in the user's home.
func UpdateBinary() error { return updateBinaryOn(runtime.GOOS) }

// updateBinaryOn is the testable half of UpdateBinary: the platform refusal is
// the first operation, so direct package callers cannot write a binary on an
// unsupported host merely because the CLI guard was bypassed.
func updateBinaryOn(goos string) error {
	if !platformSupported(goos) {
		return unsupportedPlatformError(goos)
	}

	t, err := resolveTarget()
	if err != nil {
		return err
	}

	return installBinary(binaryPathFor(t), t)
}

// UnitStale reports whether the installed unit differs from what this version
// would write. A missing file counts as stale.
func UnitStale() (bool, error) { return unitStaleOn(runtime.GOOS) }

// unitStaleOn is the testable half of UnitStale: the platform refusal is the
// first operation, so direct package callers cannot resolve a unit on an
// unsupported host merely because the CLI guard was bypassed.
func unitStaleOn(goos string) (bool, error) {
	if !platformSupported(goos) {
		return false, unsupportedPlatformError(goos)
	}

	t, err := resolveTarget()
	if err != nil {
		return false, err
	}

	path, want, err := expectedUnit(t)
	if err != nil {
		return false, err
	}

	return unitFileStale(path, want)
}

// unitFileStale is the filesystem-only half of UnitStale. Keeping target and
// platform discovery outside makes the contract testable without consulting a
// developer machine's real /etc/systemd state.
func unitFileStale(path, want string) (bool, error) {
	got, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	return string(got) != want, nil
}

// binaryPathFor is where the binary lives: in the target's home, so updating it
// needs no privileges. The daemon runs as that same user, so nothing is crossed.
func binaryPathFor(t target) string {
	return filepath.Join(t.home, ".local", "bin", "coagent")
}

func resolveTarget() (target, error) {
	// nosemgrep: coagent-no-direct-environment-read -- process identity, not config
	name := os.Getenv("SUDO_USER")
	if name == "" {
		current, err := user.Current()
		if err != nil {
			return target{}, fmt.Errorf("resolve current user: %w", err)
		}

		if current.Uid == "0" {
			return target{}, errors.New(
				"run this through sudo, not as root directly — the daemon runs as your login user " +
					"and root's home is not where your config lives")
		}

		return fromUser(current)
	}

	u, err := user.Lookup(name)
	if err != nil {
		return target{}, fmt.Errorf("look up %s: %w", name, err)
	}

	return fromUser(u)
}

func fromUser(u *user.User) (target, error) {
	t := target{name: u.Username, home: u.HomeDir}

	if _, err := fmt.Sscan(u.Uid, &t.uid); err != nil {
		return target{}, fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}

	if _, err := fmt.Sscan(u.Gid, &t.gid); err != nil {
		return target{}, fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}

	return t, nil
}
