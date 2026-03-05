package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}

	t.Run("non-bash returns empty", func(t *testing.T) {
		t.Setenv("SHELL", "/usr/bin/zsh")
		assert.Empty(t, resolveShell())
	})

	t.Run("bash resolves", func(t *testing.T) {
		t.Setenv("SHELL", bash)
		got := resolveShell()
		require.NotEmpty(t, got)
		assert.Equal(t, "bash", filepath.Base(got))
	})

	t.Run("empty falls back to PATH bash", func(t *testing.T) {
		t.Setenv("SHELL", "")
		got := resolveShell()
		require.NotEmpty(t, got)
		assert.Equal(t, "bash", filepath.Base(got))
	})
}

func TestNew_NonBashShellDisablesSnapshotting(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")

	p := New()
	assert.Empty(t, p.Shell())
	assert.Empty(t, p.Snapshot(context.Background(), t.TempDir()))
}

func TestShellQuote(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"plain":        {in: "abc", want: "'abc'"},
		"space":        {in: "a b", want: "'a b'"},
		"single quote": {in: "a'b", want: `'a'\''b'`},
		"semicolon":    {in: "a;b", want: "'a;b'"},
		"subshell":     {in: "$(x)", want: "'$(x)'"},
		"empty":        {in: "", want: "''"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, shellQuote(tt.in))
		})
	}
}

func TestFilterReadonly(t *testing.T) {
	in := "declare -x PATH=\"/bin\"\n" +
		"declare -rx SHELLOPTS=\"braceexpand\"\n" +
		"declare -irx COUNT=\"3\"\n" +
		"declare -ax ARR=([0]=\"a\")\n"

	out := string(filterReadonly([]byte(in)))

	assert.Contains(t, out, `declare -x PATH="/bin"`)
	assert.Contains(t, out, "declare -ax ARR", "exported array (no r) must survive")
	assert.NotContains(t, out, "SHELLOPTS", "readonly-exported must be dropped")
	assert.NotContains(t, out, "COUNT", "integer readonly-exported (declare -irx) must be dropped")
}

func TestParseDump(t *testing.T) {
	build := func(prefix, exports string) []byte {
		return []byte("rc banner noise\n\n" + dumpMarker + "\n" + prefix +
			"\n" + exportMarker + "\n" + exports)
	}

	t.Run("strips rc noise + readonly exports, keeps functions", func(t *testing.T) {
		// A column-0 `declare -rx` inside a heredoc function body must survive:
		// it lives in the declare -f section, which the readonly filter never sees.
		prefix := "shopt -s extglob\nf () \n{ \n    cat <<EOF\ndeclare -rx INSIDE=1\nEOF\n}"
		exports := "declare -x PATH=\"/bin\"\ndeclare -rx SHELLOPTS=\"x\"\n"

		got, err := parseDump(build(prefix, exports))
		require.NoError(t, err)

		out := string(got)
		assert.Contains(t, out, `declare -x PATH="/bin"`)
		assert.NotContains(t, out, "SHELLOPTS")
		assert.Contains(t, out, "declare -rx INSIDE=1", "heredoc-body line must survive unfiltered")
		assert.Contains(t, out, "shopt -s extglob")
	})

	t.Run("missing dump marker errors", func(t *testing.T) {
		_, err := parseDump([]byte("no marker here\n"))
		require.Error(t, err)
	})

	t.Run("missing export marker errors", func(t *testing.T) {
		_, err := parseDump([]byte("x\n" + dumpMarker + "\nno export section\n"))
		require.Error(t, err)
	})
}

func TestWrapExec_EmptyArgvErrors(t *testing.T) {
	p := &provider{}
	_, err := p.WrapExec(context.Background(), t.TempDir(), nil, nil)
	require.Error(t, err)
}

func TestWrapExec_NoSnapshotPlainExec(t *testing.T) {
	p := &provider{} // shell == "" → no snapshot

	cmd, err := p.WrapExec(context.Background(), "/tmp", []string{"echo", "hi"}, []string{"FOO=bar"})
	require.NoError(t, err)

	assert.Equal(t, []string{"echo", "hi"}, cmd.Args)
	assert.Equal(t, "/tmp", cmd.Dir)
	assert.Contains(t, cmd.Env, "FOO=bar")
}

func TestWrapExec_WithSnapshotSourcesAndExecs(t *testing.T) {
	p := fakeProvider(t)

	wd := t.TempDir()
	cmd, err := p.WrapExec(context.Background(), wd, []string{"/usr/bin/gopls", "-v"}, []string{"K=V"})
	require.NoError(t, err)

	assert.Equal(t, "bash", filepath.Base(cmd.Path))
	require.Len(t, cmd.Args, 3)
	assert.Equal(t, "-c", cmd.Args[1])
	assert.Contains(t, cmd.Args[2], "source '")
	assert.Contains(t, cmd.Args[2], "; exec '/usr/bin/gopls' '-v'")
	assert.Equal(t, wd, cmd.Dir)
	assert.Contains(t, cmd.Env, "K=V")
}

func TestEnsureCacheDir_Mode0700(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("UserCacheDir ignores XDG_CACHE_HOME on darwin")
	}

	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p := &provider{instanceID: "inst", ttl: time.Minute}
	dir, err := p.ensureCacheDir()
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// fakeProvider returns a provider whose capture is a fast stub, with the cache
// dir pinned to a temp dir so tests never touch the real UserCacheDir.
func fakeProvider(t *testing.T) *provider {
	t.Helper()

	p := &provider{
		shell:    "/bin/bash",
		salt:     []byte("deterministic-test-salt"),
		ttl:      5 * time.Minute,
		cacheDir: t.TempDir(),
	}
	p.captureFn = func(context.Context, string) ([]byte, error) {
		return []byte("declare -x PATH=\"/bin\"\n"), nil
	}

	return p
}
