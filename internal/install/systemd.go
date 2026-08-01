package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const unitName = "coagent.service"

const systemUnitDir = "/etc/systemd/system"

// unitTemplate keeps the runaway guards the hand-written unit carried: a memory
// ceiling with OOMPolicy=stop so a leak dies instead of taking the box down, a
// CPU quota, and a start-limit burst so a crash loop ends in `failed` rather
// than spinning forever.
var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=coagent daemon
Documentation=https://github.com/pilat/coagent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User={{.User}}
ExecStart={{.BinaryPath}} daemon
Restart=on-failure
RestartSec=5
MemoryMax=4G
OOMPolicy=stop
CPUQuota=50%

[Install]
WantedBy=multi-user.target
`))

type unitParams struct {
	User       string
	BinaryPath string
}

var _ Manager = (*systemdManager)(nil)

type systemdManager struct {
	unitPath   string
	binaryPath string
	target     target
}

// renderUnit produces the unit file text. Exported through the golden test only
// — the shape of what lands in /etc is worth pinning.
func renderUnit(p unitParams) (string, error) {
	var sb strings.Builder

	if err := unitTemplate.Execute(&sb, p); err != nil {
		return "", fmt.Errorf("render unit: %w", err)
	}

	return sb.String(), nil
}

func newSystemd() (Manager, error) {
	t, err := resolveTarget()
	if err != nil {
		return nil, err
	}

	return &systemdManager{
		unitPath:   filepath.Join(systemUnitDir, unitName),
		binaryPath: binaryPathFor(t),
		target:     t,
	}, nil
}

// expectedUnit renders what this version would install, for the drift check.
func expectedUnit(t target) (string, string, error) {
	unit, err := renderUnit(unitParams{User: t.name, BinaryPath: binaryPathFor(t)})
	if err != nil {
		return "", "", err
	}

	return filepath.Join(systemUnitDir, unitName), unit, nil
}

func (m *systemdManager) Info() Info {
	ctx := context.Background()

	info := Info{
		Platform:   platformLinux,
		Supported:  true,
		Installed:  exists(m.unitPath),
		Scope:      scopeSystem,
		UnitName:   unitName,
		UnitPath:   m.unitPath,
		BinaryPath: m.binaryPath,
		RunAsUser:  m.target.name,
		LogCommand: "journalctl -u coagent -f",
	}

	if info.Installed {
		info.Active = succeeds(ctx, "systemctl", "is-active", "--quiet", unitName)
	}

	return info
}

func (m *systemdManager) Plan() Plan {
	return Plan{
		Description: "systemd system unit: " + unitName,
		UnitPath:    m.unitPath,
		BinaryPath:  m.binaryPath,
		RunAsUser:   m.target.name,
		Command:     "sudo coagent daemon install",
		NeedsRoot:   true,
	}
}

func (m *systemdManager) Install(ctx context.Context) error {
	if err := installBinary(m.binaryPath, m.target); err != nil {
		return err
	}

	unit, err := renderUnit(unitParams{User: m.target.name, BinaryPath: m.binaryPath})
	if err != nil {
		return err
	}

	if err := writeFileAtomic(m.unitPath, unit); err != nil {
		return err
	}

	if err := m.reload(ctx); err != nil {
		return err
	}

	// enable --now covers both first install and reinstall-over-stopped; a
	// running unit still needs the restart to pick up the new binary.
	if err := run(ctx, "systemctl", "enable", "--now", unitName); err != nil {
		return err
	}

	return run(ctx, "systemctl", "restart", unitName)
}

func (m *systemdManager) Uninstall(ctx context.Context) error {
	// Errors are ignored deliberately: uninstall must finish on a half-installed
	// or already-stopped service, and "not loaded" is exactly that state.
	_ = run(ctx, "systemctl", "disable", "--now", unitName)
	_ = run(ctx, "systemctl", "reset-failed", unitName)

	if err := os.Remove(m.unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", m.unitPath, err)
	}

	if err := m.reload(ctx); err != nil {
		return err
	}

	if err := os.Remove(m.binaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", m.binaryPath, err)
	}

	return nil
}

func (m *systemdManager) Start(ctx context.Context) error {
	return run(ctx, "systemctl", "start", unitName)
}

func (m *systemdManager) Stop(ctx context.Context) error {
	return run(ctx, "systemctl", "stop", unitName)
}

func (m *systemdManager) Restart(ctx context.Context) error {
	return run(ctx, "systemctl", "restart", unitName)
}

func (m *systemdManager) reload(ctx context.Context) error {
	return run(ctx, "systemctl", "daemon-reload")
}
