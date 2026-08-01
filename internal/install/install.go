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

// Lifecycle verbs. The CLI, the unit file and the onboarding sudo handoff agree
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
	// unitMode is what the unit and the plist get. launchd refuses a
	// group-writable plist outright.
	unitMode = 0o644
)

// Platform names, used where the answer travels to a UI rather than to a switch.
const (
	platformLinux  = "linux"
	platformDarwin = "darwin"
)

// scopeSystem is the only scope there is: both platforms register a system
// service that drops to the target user.
const scopeSystem = "system"

var errUnsupported = errors.New("service installation is supported on linux (systemd) and macOS (launchd) only")

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

// New returns the service manager for this platform. There is one install mode
// per platform: a system unit that drops to the target user.
func New() (Manager, error) {
	switch runtime.GOOS {
	case platformLinux:
		return newSystemd()
	case platformDarwin:
		return newLaunchd()
	default:
		return nil, errUnsupported
	}
}

// UpdateBinary replaces the installed binary with the running one. It needs no
// privileges — that is the whole point of keeping the binary in the user's home.
func UpdateBinary() error {
	t, err := resolveTarget()
	if err != nil {
		return err
	}

	return installBinary(binaryPathFor(t), t)
}

// UnitStale reports whether the installed unit/plist differs from what this
// version would write. A missing file counts as stale.
func UnitStale() (bool, error) {
	t, err := resolveTarget()
	if err != nil {
		return false, err
	}

	var (
		path string
		want string
	)

	switch runtime.GOOS {
	case platformLinux:
		path, want, err = expectedUnit(t)
	case platformDarwin:
		path, want, err = expectedPlist(t)
	default:
		return false, errUnsupported
	}

	if err != nil {
		return false, err
	}

	return unitFileStale(path, want)
}

// unitFileStale is the filesystem-only half of UnitStale. Keeping target and
// platform discovery outside makes the contract testable without consulting a
// developer machine's real /etc/systemd or /Library/LaunchDaemons state.
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
