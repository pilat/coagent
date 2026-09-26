package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/sandboxnet"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		handled, err := runModes(
			sandboxnet.RunSetupMode,
			sandboxnet.RunJoinMode,
			bashsandbox.RunDialMode,
			runOpaqueFixture,
		)(
			os.Args[1:],
		)
		if handled {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestProbeNetworkSetup_ExercisesJoinAndResolverMount(t *testing.T) {
	if os.Getenv("COAGENT_ROUTED_PROBE_FIXTURE") != "1" {
		if os.Getenv("CI") != "true" {
			t.Skip("native network preflight runs in CI")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		//nolint:gosec // Fixed argv; re-invokes this test binary.
		command := exec.CommandContext(ctx, "sudo", "-n", "--", "unshare", "--net", "--", "/usr/bin/setpriv",
			"--reuid="+strconv.Itoa(os.Getuid()), "--regid="+strconv.Itoa(os.Getgid()), "--clear-groups",
			"--inh-caps=+net_admin,+sys_admin", "--ambient-caps=+net_admin,+sys_admin", "env",
			"CI=true", "COAGENT_ROUTED_PROBE_FIXTURE=1", os.Args[0],
			"-test.run=^TestProbeNetworkSetup_ExercisesJoinAndResolverMount$", "-test.timeout=30s", "-test.v")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	require.NoError(t, exec.CommandContext(t.Context(), "ip", "link", "set", "lo", "up").Run())
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0))
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0))
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, probeNetworkSetup(t.Context()))
}

func TestNetworkOwner_LeaseIdleWorkloadAndRetirement(t *testing.T) {
	created := 0
	closed := 0
	workload := false
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			created++
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error { closed++; return nil },
			}, nil
		},
		hasWorkloads: func(*networkGeneration) bool { return workload },
	}
	policy := sandboxpolicy.Policy{Digest: "ordinary"}
	first, err := owner.Acquire(t.Context(), 7, policy)
	require.NoError(t, err)
	second, err := owner.Acquire(t.Context(), 7, policy)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	_, err = owner.Acquire(t.Context(), 7, sandboxpolicy.Policy{Digest: "shielded"})
	require.ErrorContains(t, err, "another network policy")
	first.Release()
	owner.reapIdle(t.Context(), time.Now().Add(11*time.Minute))
	assert.Equal(t, 0, closed, "a second stack still holds the generation")
	second.Release()
	workload = true
	owner.reapIdle(t.Context(), time.Now().Add(11*time.Minute))
	assert.Equal(t, 0, closed, "a background workload keeps the namespace alive")
	workload = false
	owner.reapIdle(t.Context(), time.Now().Add(22*time.Minute))
	assert.Equal(t, 0, closed, "idle time begins when the last workload is observed gone")
	owner.reapIdle(t.Context(), time.Now().Add(33*time.Minute))
	assert.Equal(t, 1, closed)
	assert.Empty(t, owner.entries)

	third, err := owner.Acquire(t.Context(), 7, sandboxpolicy.Policy{Digest: "shielded"})
	require.NoError(t, err)
	assert.Equal(t, 2, created)
	require.NoError(t, owner.Retire(t.Context(), 7))
	third.Release()
	assert.Equal(t, 2, closed)
}

func TestNetworkOwner_FailedRetirementBlocksReplacement(t *testing.T) {
	stopErr := fmt.Errorf("native route cutoff failed")
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error { return stopErr },
			}, nil
		},
	}
	_, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
	require.NoError(t, err)
	require.ErrorIs(t, owner.Retire(t.Context(), 1), stopErr)
	_, err = owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "new"})
	require.ErrorIs(t, err, stopErr)
	_, err = owner.Acquire(t.Context(), 2, sandboxpolicy.Policy{Digest: "unrelated"})
	require.NoError(t, err, "retirement failure is scoped to its root")
	require.ErrorIs(t, owner.Stop(t.Context()), stopErr)
}

func TestNetworkGeneration_ConcurrentCloseJoinsCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	stopErr := fmt.Errorf("cleanup result")
	var calls atomic.Int32
	generation := &networkGeneration{stopFn: func(context.Context) error {
		calls.Add(1)
		close(entered)
		<-release
		return stopErr
	}}
	first := make(chan error, 1)
	go func() { first <- generation.close(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not begin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, generation.close(ctx), context.DeadlineExceeded)
	close(release)
	require.ErrorIs(t, <-first, stopErr)
	require.ErrorIs(t, generation.close(t.Context()), stopErr)
	require.Equal(t, int32(1), calls.Load())
}

func TestNetworkOwner_CutoffBlocksAcquireUntilRetirement(t *testing.T) {
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error { return nil },
			}, nil
		},
	}
	_, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
	require.NoError(t, err)
	require.NoError(t, owner.Cutoff(t.Context(), 1))
	_, err = owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
	require.ErrorContains(t, err, "revoked")
	require.NoError(t, owner.Retire(t.Context(), 1))
	_, err = owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "new"})
	require.NoError(t, err)
	require.NoError(t, owner.Stop(t.Context()))
}

func TestNetworkOwner_RetireDuringStartupFencesTheGeneration(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	var closed atomic.Int32
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			if rootID == 1 {
				close(entered)
				<-resume
			}
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error { closed.Add(1); return nil },
			}, nil
		},
	}
	type result struct{ err error }
	first := make(chan result, 1)
	go func() {
		_, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
		first <- result{err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first startup did not begin")
	}
	second := make(chan result, 1)
	go func() {
		_, err := owner.Acquire(t.Context(), 2, sandboxpolicy.Policy{Digest: "other"})
		second <- result{err: err}
	}()
	select {
	case got := <-second:
		require.NoError(t, got.err)
	case <-time.After(2 * time.Second):
		t.Fatal("an unrelated root was blocked by startup")
	}
	retired := make(chan error, 1)
	go func() { retired <- owner.Retire(t.Context(), 1) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		owner.mu.Lock()
		started := owner.retiring[1] != nil
		owner.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retirement did not fence the root")
		}
		runtime.Gosched()
	}
	close(resume)
	select {
	case err := <-retired:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("retirement did not join the stale startup")
	}
	select {
	case got := <-first:
		require.ErrorContains(t, got.err, "retired during startup")
	case <-time.After(2 * time.Second):
		t.Fatal("retired startup did not settle")
	}
	assert.Equal(t, int32(1), closed.Load())
	assert.Nil(t, owner.entries[1])
	require.NoError(t, owner.Stop(t.Context()))
}

func TestNetworkOwner_RetirementBlocksNewPolicyUntilOldFlowCloses(t *testing.T) {
	stopping := make(chan struct{})
	allowStop := make(chan struct{})
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			generation := &networkGeneration{rootID: rootID, digest: policy.Digest}
			if policy.Digest == "old" {
				generation.stopFn = func(context.Context) error {
					close(stopping)
					<-allowStop
					return nil
				}
			} else {
				generation.stopFn = func(context.Context) error { return nil }
			}
			return generation, nil
		},
	}
	first, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
	require.NoError(t, err)
	first.Release()
	retired := make(chan error, 1)
	go func() { retired <- owner.Retire(t.Context(), 1) }()
	select {
	case <-stopping:
	case <-time.After(2 * time.Second):
		t.Fatal("old generation did not begin retirement")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = owner.Acquire(waitCtx, 1, sandboxpolicy.Policy{Digest: "new"})
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(allowStop)
	require.NoError(t, <-retired)
	_, err = owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "new"})
	require.NoError(t, err)
	require.NoError(t, owner.Stop(t.Context()))
}

func TestNetworkOwner_StopJoinsStartupBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	closed := make(chan struct{})
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			close(entered)
			<-resume
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error { close(closed); return nil },
			}, nil
		},
	}
	acquired := make(chan error, 1)
	go func() {
		_, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
		acquired <- err
	}()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- owner.Stop(t.Context()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		owner.mu.Lock()
		fenced := owner.stopped
		owner.mu.Unlock()
		if fenced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop did not fence acquisition")
		}
		runtime.Gosched()
	}
	close(resume)
	require.ErrorContains(t, <-acquired, "retired during startup")
	require.NoError(t, <-stopped)
	select {
	case <-closed:
	default:
		t.Fatal("stop returned before stale generation closed")
	}
}

func TestNetworkOwner_StopJoinsIdleRetirement(t *testing.T) {
	stopping := make(chan struct{})
	allowStop := make(chan struct{})
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), done: make(chan struct{}),
		start: func(_ context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
			return &networkGeneration{
				rootID: rootID, digest: policy.Digest,
				stopFn: func(context.Context) error {
					close(stopping)
					<-allowStop
					return nil
				},
			}, nil
		},
		hasWorkloads: func(*networkGeneration) bool { return false },
	}
	lease, err := owner.Acquire(t.Context(), 1, sandboxpolicy.Policy{Digest: "old"})
	require.NoError(t, err)
	lease.Release()
	start := time.Now()
	owner.reapIdle(t.Context(), start)
	reaped := make(chan struct{})
	go func() {
		owner.reapIdle(t.Context(), start.Add(11*time.Minute))
		close(reaped)
	}()
	<-stopping
	stopped := make(chan error, 1)
	go func() { stopped <- owner.Stop(t.Context()) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while the idle generation was still closing")
	case <-time.After(50 * time.Millisecond):
	}
	close(allowStop)
	<-reaped
	require.NoError(t, <-stopped)
}
