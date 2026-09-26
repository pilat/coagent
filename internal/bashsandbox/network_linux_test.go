//go:build linux

package bashsandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxnet"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// TestMain lets the test binary stand in for the daemon binary: it handles the
// hidden setup subcommand exactly as the daemon would.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandboxnet.SetupCommand {
		runSetupChild()

		return
	}
	if len(os.Args) > 1 && os.Args[1] == sandboxnet.JoinCommand {
		_, err := sandboxnet.RunJoinMode(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == DialCommand {
		_, err := RunDialMode(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func TestNetworkJoinedRunner_AdmittedHostAliasAndPrivateLoopback(t *testing.T) {
	if os.Getenv("COAGENT_ROUTED_RUNNER_FIXTURE") != "1" {
		if os.Getenv("CI") != "true" {
			t.Skip("native routed sandbox scenarios run in CI")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		//nolint:gosec // Fixed argv; re-invokes this test binary.
		command := exec.CommandContext(ctx, "sudo", "-n", "--", "unshare", "--net", "--", "/usr/bin/setpriv",
			"--reuid="+strconv.Itoa(os.Getuid()), "--regid="+strconv.Itoa(os.Getgid()), "--clear-groups",
			"--inh-caps=+net_admin,+sys_admin", "--ambient-caps=+net_admin,+sys_admin", "env",
			"CI=true", "COAGENT_ROUTED_RUNNER_FIXTURE=1", os.Args[0],
			"-test.run=^TestNetworkJoinedRunner_AdmittedHostAliasAndPrivateLoopback$", "-test.timeout=30s", "-test.v")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	require.NoError(t, exec.CommandContext(t.Context(), "ip", "link", "set", "lo", "up").Run())
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0))
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0))
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal(err)
	}
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	hostServer := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "host-only")
		}),
	}
	t.Cleanup(func() { _ = hostServer.Close() })
	go func() { _ = hostServer.Serve(listener) }()
	port := listener.Addr().(*net.TCPAddr).Port
	ipv6Listener, ipv6Err := net.Listen("tcp", "[::1]:0")
	if ipv6Err != nil {
		t.Logf("host IPv6 loopback unavailable; namespace IPv6 is checked by the DNS probe: %v", ipv6Err)
	}
	ports := []int{port}
	ipv6Port := 0
	if ipv6Listener != nil {
		ipv6Server := &http.Server{
			ReadHeaderTimeout: time.Second,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "host-v6")
			}),
		}
		t.Cleanup(func() { _ = ipv6Server.Close() })
		ipv6Port = ipv6Listener.Addr().(*net.TCPAddr).Port
		ports = append(ports, ipv6Port)
		go func() { _ = ipv6Server.Serve(ipv6Listener) }()
	}
	udpEcho, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = udpEcho.Close() })
	go serveProbeUDPEcho(udpEcho)
	udpPort := udpEcho.LocalAddr().(*net.UDPAddr).Port
	catalog := testCatalog(t, map[string]sandboxpolicy.Profile{
		"host-test": {Network: []sandboxpolicy.Network{{
			Address: sandboxpolicy.HostLoopback, Protocol: sandboxpolicy.ProtocolTCP,
			Ports: ports, Type: sandboxpolicy.LevelBasic,
		}, {
			Address: sandboxpolicy.HostLoopback, Protocol: sandboxpolicy.ProtocolUDP,
			Ports: []int{udpPort}, Type: sandboxpolicy.LevelBasic,
		}}},
	})
	compiled, err := sandboxpolicy.Compile(catalog, testPolicyRequest(t, project))
	require.NoError(t, err)
	self, err := os.Executable()
	require.NoError(t, err)
	setup, err := StartNetworkSetup(context.WithoutCancel(t.Context()), NetworkSetupConfig{
		Binary: self, ReceiveTimeout: 5 * time.Second,
	})
	if errors.Is(err, sandboxnet.ErrSetupUnsupported) && os.Getenv("CI") == "" {
		t.Skip(err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = setup.Close(); require.NoError(t, setup.Cmd.Wait()) })
	open := func(path string) *os.File {
		file, openErr := os.Open(path)
		require.NoError(t, openErr)
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	userNS := open(fmt.Sprintf("/proc/%d/ns/user", setup.Cmd.Process.Pid))
	netNS := open(fmt.Sprintf("/proc/%d/ns/net", setup.Cmd.Process.Pid))
	binaryFile := open(self)
	configFile := func(name, content string) *os.File {
		file, createErr := os.CreateTemp(t.TempDir(), name)
		require.NoError(t, createErr)
		_, createErr = file.WriteString(content)
		require.NoError(t, createErr)
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	link := sandboxnet.DefaultLink()
	resolver := configFile("resolver", "nameserver "+link.GatewayIPv4.String()+"\n")
	hosts := configFile("hosts", fmt.Sprintf("127.0.0.1 localhost\n::1 localhost\n%s %s\n%s %s\n",
		link.HostAliasIPv4, sandboxnet.HostAlias, link.HostAliasIPv6, sandboxnet.HostAlias))
	classifier, err := sandboxnet.NewClassifier(nil, compiled.Network)
	require.NoError(t, err)
	dnsTCP, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dnsTCP.Close() })
	dnsUDP, err := net.ListenPacket("udp", dnsTCP.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = dnsUDP.Close() })
	go serveProbeDNSUDP(dnsUDP)
	go serveProbeDNSTCP(dnsTCP)
	gateway, err := sandboxnet.NewRouter(t.Context(), netNS, sandboxnet.Config{
		Link: link, Classifier: classifier,
		DNSUpstreams: []string{dnsTCP.Addr().String()},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, gateway.Stop(ctx))
	})
	process, err := buildProcessPolicy(
		Config{Enabled: true, Policy: compiled, WorkDir: project, SessionKey: "join-test"},
	)
	require.NoError(t, err)
	runner, err := newEnabledRunnerWithNetwork(process, &NetworkLink{
		UserNS: userNS, NetNS: netNS,
		BinaryFile: binaryFile, Resolver: resolver, Hosts: hosts,
	})
	require.NoError(t, err)
	cmd, err := runner.BashCommand(
		t.Context(),
		fmt.Sprintf("curl --noproxy '*' -fsS http://%s:%d/", sandboxnet.HostAlias, port),
		project,
	)
	require.NoError(t, err)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Equal(t, "host-only", string(output))
	if ipv6Listener != nil {
		cmd, err = runner.BashCommand(
			t.Context(),
			fmt.Sprintf("curl -6 --noproxy '*' -fsS http://%s:%d/", sandboxnet.HostAlias, ipv6Port),
			project,
		)
		require.NoError(t, err)
		output, err = cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		assert.Equal(t, "host-v6", string(output))
	}
	cmd, err = runner.BashCommand(
		t.Context(),
		fmt.Sprintf("curl --noproxy '*' --max-time 2 -fsS http://127.0.0.1:%d/", port),
		project,
	)
	require.NoError(t, err)
	_, err = cmd.CombinedOutput()
	require.Error(t, err, "host loopback must not appear at the sandbox's localhost")
	cmd, err = runner.BashCommand(t.Context(),
		"test ! -e /proc/self/fd/4 && test ! -e /proc/self/fd/5 && "+
			"while read -r key value; do if test \"$key\" = CapEff:; then "+
			"test \"$value\" = 0000000000000000; exit; fi; done < /proc/self/status", project)
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "the entry stage must close setup descriptors and drop capabilities: %s", output)
	cmd, err = runner.Command(t.Context(), procexec.Request{
		Path: EntryBinaryPath, Args: []string{"-test.run=^TestNetworkDNSProbeChild$"}, WorkDir: project,
		Env: []string{"COAGENT_DNS_PROBE=1"},
	})
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
	require.NoError(t, err, "DNS over IPv4/UDP and IPv6/TCP must cross the namespace: %s", output)
	cmd, err = runner.Command(t.Context(), procexec.Request{
		Path: EntryBinaryPath, Args: []string{"-test.run=^TestNetworkTransportProbeChild$"}, WorkDir: project,
		Env: []string{"COAGENT_UDP_PROBE=" + net.JoinHostPort(link.HostAliasIPv4.String(), strconv.Itoa(udpPort))},
	})
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
	require.NoError(t, err, "UDP payloads must survive the real namespace and TUN: %s", output)
}

func serveProbeUDPEcho(listener net.PacketConn) {
	buffer := make([]byte, 65535)
	for {
		n, peer, err := listener.ReadFrom(buffer)
		if err != nil {
			return
		}
		_, _ = listener.WriteTo(buffer[:n], peer)
	}
}

func TestNetworkTransportProbeChild(t *testing.T) {
	address := os.Getenv("COAGENT_UDP_PROBE")
	if address == "" {
		t.Skip("namespace UDP probe process")
	}
	conn, err := net.Dial("udp4", address) //nolint:gosec // The parent test passes its confined UDP echo listener.
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	raw, err := conn.(*net.UDPConn).SyscallConn()
	require.NoError(t, err)
	var socketErr error
	require.NoError(t, raw.Control(func(fd uintptr) {
		// Permit fragmentation so the probe exercises payloads larger than the TUN MTU.
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DONT)
	}))
	require.NoError(t, socketErr)
	for _, size := range []int{0, 1, 32769, 60000, 65507} {
		require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
		payload := bytes.Repeat([]byte{byte(size % 251)}, size)
		_, err = conn.Write(payload)
		require.NoError(t, err)
		buffer := make([]byte, 65535)
		n, readErr := conn.Read(buffer)
		require.NoError(t, readErr, "size=%d", size)
		require.Equal(t, payload, buffer[:n])
	}
}

func serveProbeDNSUDP(listener net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, peer, err := listener.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		_, _ = listener.WriteTo(probeDNSReply(buf[:n]), peer)
	}
}

func serveProbeDNSTCP(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			length := make([]byte, 2)
			if _, err := io.ReadFull(conn, length); err != nil {
				return
			}
			query := make([]byte, binary.BigEndian.Uint16(length))
			if _, err := io.ReadFull(conn, query); err != nil || len(query) < 12 {
				return
			}
			reply := probeDNSReply(query)
			binary.BigEndian.PutUint16(length, uint16(len(reply)))
			frame := append(length[:0:0], length...)
			_, _ = conn.Write(append(frame, reply...))
		}()
	}
}

func probeDNSReply(query []byte) []byte {
	reply := append([]byte(nil), query...)
	reply[2], reply[3] = 0x81, 0x80
	return reply
}

func TestNetworkDNSProbeChild(t *testing.T) {
	if os.Getenv("COAGENT_DNS_PROBE") != "1" {
		t.Skip("namespace DNS probe process")
	}
	query := []byte{
		0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0,
		7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1,
	}
	for _, test := range []struct {
		network, address string
		tcp              bool
	}{
		{"udp4", net.JoinHostPort(sandboxnet.DefaultLink().GatewayIPv4.String(), "53"), false},
		{"tcp6", net.JoinHostPort(sandboxnet.DefaultLink().GatewayIPv6.String(), "53"), true},
	} {
		conn, err := net.DialTimeout(test.network, test.address, 5*time.Second)
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		if test.tcp {
			frame := make([]byte, 2)
			binary.BigEndian.PutUint16(frame, uint16(len(query)))
			request := append(frame[:0:0], frame...)
			_, err = conn.Write(append(request, query...))
		} else {
			_, err = conn.Write(query)
		}
		require.NoError(t, err)
		if test.tcp {
			length := make([]byte, 2)
			_, err = io.ReadFull(conn, length)
			require.NoError(t, err)
			require.Equal(t, len(query), int(binary.BigEndian.Uint16(length)))
		}
		reply := make([]byte, len(query))
		_, err = io.ReadFull(conn, reply)
		require.NoError(t, err)
		require.Equal(t, []byte{0x12, 0x34, 0x81, 0x80}, reply[:4])
		require.NoError(t, conn.Close())
	}
	for _, address := range []string{
		sandboxnet.DefaultLink().GatewayIPv4.String(),
		sandboxnet.DefaultLink().GatewayIPv6.String(),
	} {
		probePairedDNS(t, address, query)
	}
}

func probePairedDNS(t *testing.T, address string, query []byte) {
	t.Helper()
	conn, err := net.DialTimeout("udp", net.JoinHostPort(address, "53"), 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	second := append([]byte(nil), query...)
	second[1]++
	second[len(second)-3] = 28
	_, err = conn.Write(query)
	require.NoError(t, err)
	_, err = conn.Write(second)
	require.NoError(t, err)
	var ids []uint16
	for range 2 {
		reply := make([]byte, 4096)
		n, readErr := conn.Read(reply)
		require.NoError(t, readErr, "paired A/AAAA replies must precede resolver retry")
		require.GreaterOrEqual(t, n, 12)
		ids = append(ids, binary.BigEndian.Uint16(reply[:2]))
	}
	assert.ElementsMatch(t, []uint16{0x1234, 0x1235}, ids)
}

// runSetupChild runs the same hidden mode as the compiled daemon.
func runSetupChild() {
	config, err := sandboxnet.ParseSetupArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse setup args: %v\n", err)

		os.Exit(1)
	}

	if runErr := sandboxnet.RunSetup(config); runErr != nil {
		fmt.Fprintf(os.Stderr, "setup child failed: %v\n", runErr)

		if errors.Is(runErr, sandboxnet.ErrSetupUnsupported) {
			os.Exit(sandboxnet.SetupUnsupportedExit)
		}

		os.Exit(1)
	}
}

func TestNetworkSetup_HoldsItsOwnNamespaceUntilTheSocketCloses(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setup, err := StartNetworkSetup(ctx, NetworkSetupConfig{Binary: self, ReceiveTimeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, sandboxnet.ErrSetupUnsupported) && os.Getenv("CI") == "" {
			t.Skip("this environment cannot create a user namespace")
		}

		require.NoError(t, err, "CI must provide rootless namespaces")
	}

	assert.GreaterOrEqual(t, setup.FD, 0, "readiness is the namespace descriptor itself")

	child, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", setup.Cmd.Process.Pid))
	require.NoError(t, err)

	parent, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)

	assert.NotEqual(t, parent, child, "the confined tree must not share the daemon's network namespace")

	// Closing the setup socket is the retirement signal; nothing else stops it.
	require.NoError(t, setup.Close())
	require.NoError(t, setup.Cmd.Wait())
}

// TestNetworkPrefix_EntersTheTreeNamespaces pins the join shape: bubblewrap
// enters the owning user namespace by descriptor, the pinned network namespace
// handle is exposed read-only, and the entry stage's own binary is mounted so
// that stage can run at all.
func TestNetworkPrefix_EntersTheTreeNamespaces(t *testing.T) {
	userNS, err := os.CreateTemp(t.TempDir(), "userns")
	require.NoError(t, err)
	t.Cleanup(func() { _ = userNS.Close() })

	link := &NetworkLink{
		UserNS: userNS, NetNS: userNS, BinaryFile: userNS, Resolver: userNS, Hosts: userNS,
	}

	args := networkPrefix(link)
	assert.Contains(t, args, "--userns")
	assert.Contains(t, args, strconv.Itoa(userNSDescriptor))
	assert.Contains(t, args, "--ro-bind-fd")
	assert.Contains(t, args, EntryBinaryPath)

	files, err := networkExtraFiles(link)
	require.NoError(t, err)
	require.Len(t, files, 5, "the namespace and runtime descriptors are inherited")
	assert.NotSame(t, userNS, files[0], "commands own descriptor duplicates")
	for _, file := range files {
		require.NoError(t, file.Close())
	}
	_, err = userNS.Stat()
	require.NoError(t, err, "closing command descriptors must not close the generation")
}

func TestNetworkPrefix_CreatesPrivateEntryBeforeReadOnlyRemount(t *testing.T) {
	runner := &bubblewrapRunner{network: &NetworkLink{}}
	args := runner.prefix("/project", mountPlan{}, mountPlan{})

	directory := slices.Index(args, "/run/coagent")
	entry := slices.Index(args, EntryBinaryPath)
	readonly := slices.Index(args, "--remount-ro")
	require.NotEqual(t, -1, directory)
	require.NotEqual(t, -1, entry)
	require.NotEqual(t, -1, readonly)
	assert.Less(t, directory, entry)
	assert.Less(t, entry, readonly)
}

func TestJoinStage_PrecedesTheCommand(t *testing.T) {
	link := &NetworkLink{}

	stage := joinStage(link)
	require.Len(t, stage, 5)
	assert.Equal(t, EntryBinaryPath, stage[0])
	assert.Equal(t, sandboxnet.JoinCommand, stage[1])
	assert.Equal(t, "--netns-fd", stage[2])
	assert.Equal(t, "4", stage[3])
	assert.Equal(t, "--", stage[4], "the command follows the separator")
}

func TestNetworkWrapping_IsAbsentWithoutAGeneration(t *testing.T) {
	assert.Nil(t, networkPrefix(nil))
	assert.Nil(t, joinStage(nil))
	files, err := networkExtraFiles(nil)
	require.NoError(t, err)
	assert.Nil(t, files)
	files, err = networkExtraFiles(&NetworkLink{})
	require.NoError(t, err)
	assert.Nil(t, files)
}

// TestRunner_WithoutNetworkLinkKeepsTheDaemonNamespace pins the default: no
// link, no join stage, no inherited descriptors.
func TestRunner_WithoutNetworkLinkKeepsTheDaemonNamespace(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))

	runner := testRunnerFromPolicy(t, testCompiledPolicy(t, project, false), project, "no-network")

	cmd, err := runner.BashCommand(t.Context(), "true", project)
	require.NoError(t, err)

	assert.NotContains(t, cmd.Args, "--userns")
	assert.NotContains(t, cmd.Args, sandboxnet.JoinCommand)
	assert.NotEmpty(t, cmd.ExtraFiles, "mount sources are pinned by descriptor")
}
