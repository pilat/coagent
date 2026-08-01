package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const label = "com.pilat.coagent"

const launchDaemonDir = "/Library/LaunchDaemons"

// plistTemplate writes HOME explicitly: launchd does not derive it from
// UserName for daemons, and the whole configuration lives under ~/.coagent.
// stdout and stderr are left unset on purpose — launchd routes them into the
// unified log, which is what the `log stream` command in section 1 reads.
var plistTemplate = template.Must(template.New("plist").Parse(
	`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BinaryPath}}</string>
		<string>daemon</string>
	</array>
	<key>UserName</key>
	<string>{{.User}}</string>
	<key>WorkingDirectory</key>
	<string>{{.Home}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{.Home}}</string>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`))

type plistParams struct {
	Label      string
	BinaryPath string
	User       string
	Home       string
}

var _ Manager = (*launchdManager)(nil)

type launchdManager struct {
	plistPath  string
	binaryPath string
	target     target
}

func renderPlist(p plistParams) (string, error) {
	var sb strings.Builder

	if err := plistTemplate.Execute(&sb, p); err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}

	return sb.String(), nil
}

func newLaunchd() (Manager, error) {
	t, err := resolveTarget()
	if err != nil {
		return nil, err
	}

	return &launchdManager{
		plistPath:  filepath.Join(launchDaemonDir, label+".plist"),
		binaryPath: binaryPathFor(t),
		target:     t,
	}, nil
}

// expectedPlist renders what this version would install, for the drift check.
func expectedPlist(t target) (string, string, error) {
	plist, err := renderPlist(plistParams{
		Label:      label,
		BinaryPath: binaryPathFor(t),
		User:       t.name,
		Home:       t.home,
	})
	if err != nil {
		return "", "", err
	}

	return filepath.Join(launchDaemonDir, label+".plist"), plist, nil
}

func (m *launchdManager) Info() Info {
	info := Info{
		Platform:   platformDarwin,
		Supported:  true,
		Installed:  exists(m.plistPath),
		Scope:      scopeSystem,
		UnitName:   label,
		UnitPath:   m.plistPath,
		BinaryPath: m.binaryPath,
		RunAsUser:  m.target.name,
		LogCommand: `log stream --predicate 'process == "coagent"' --level info`,
	}

	if info.Installed {
		info.Active = succeeds(context.Background(), "launchctl", "print", "system/"+label)
	}

	return info
}

func (m *launchdManager) Plan() Plan {
	return Plan{
		Description: "launchd system daemon: " + label,
		UnitPath:    m.plistPath,
		BinaryPath:  m.binaryPath,
		RunAsUser:   m.target.name,
		Command:     "sudo coagent daemon install",
		NeedsRoot:   true,
	}
}

func (m *launchdManager) Install(ctx context.Context) error {
	if err := installBinary(m.binaryPath, m.target); err != nil {
		return err
	}

	plist, err := renderPlist(plistParams{
		Label:      label,
		BinaryPath: m.binaryPath,
		User:       m.target.name,
		Home:       m.target.home,
	})
	if err != nil {
		return err
	}

	if err := writeFileAtomic(m.plistPath, plist); err != nil {
		return err
	}

	// launchd refuses to bootstrap a label it already holds, so a reinstall has
	// to bootout first. A not-loaded job makes that a no-op, hence the ignore.
	_ = run(ctx, "launchctl", "bootout", "system/"+label)

	return run(ctx, "launchctl", "bootstrap", "system", m.plistPath)
}

func (m *launchdManager) Uninstall(ctx context.Context) error {
	_ = run(ctx, "launchctl", "bootout", "system/"+label)

	if err := os.Remove(m.plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", m.plistPath, err)
	}

	if err := os.Remove(m.binaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", m.binaryPath, err)
	}

	return nil
}

func (m *launchdManager) Start(ctx context.Context) error {
	return run(ctx, "launchctl", "bootstrap", "system", m.plistPath)
}

func (m *launchdManager) Stop(ctx context.Context) error {
	return run(ctx, "launchctl", "bootout", "system/"+label)
}

// Restart uses kickstart -k, which SIGTERMs the job and starts it again in one
// step — bootout followed by bootstrap races launchd's own teardown.
func (m *launchdManager) Restart(ctx context.Context) error {
	return run(ctx, "launchctl", "kickstart", "-k", "system/"+label)
}
