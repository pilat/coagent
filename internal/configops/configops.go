package configops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/config"
)

// Service is the one semantic mutation layer, so guards, ${VAR} discipline and
// validation cannot diverge between the facades that use it.
type Service interface {
	// Stage validates ops against the config on disk, in order, and renders the
	// candidate bytes. Nothing is written; a failed verdict comes with a nil
	// Staged. A set is all-or-nothing: the bootstrap adds a provider and the
	// models that make it usable in one write, because either alone is a config
	// that cannot serve a session.
	Stage(ops ...Op) (*Staged, Verdict)
	// Commit replaces config.yaml with a staged candidate, leaving behind the
	// pending-apply marker that lets the daemon explain itself after the restart.
	Commit(staged *Staged, p Pending) Verdict
	// LoadPending reads the pending-apply marker; nil when there is none.
	LoadPending() (*Pending, error)
	// ResolvePending decides what a boot makes of a marker, rolling back when the
	// startup validation it wrapped failed. The marker stays until ClearPending.
	ResolvePending(p Pending, bootErr error) (Outcome, error)
	// ClearPending removes the marker p came from — the caller's acknowledgement
	// that the verdict landed. A marker superseded by a newer apply is left alone.
	ClearPending(p Pending) error
	// ConfigHash is the current file's sha256, "" when there is no file.
	ConfigHash() (string, error)
	// SetSecret writes one credential into the secrets file and registers it for
	// log redaction. Referenced reports whether config.yaml already resolves
	// ${name}, which makes the write a rotation the daemon must restart to see.
	SetSecret(name, value string) (bool, Verdict)
	// ConfigPath is where the config this service mutates lives.
	ConfigPath() string
}

// Staged is a validated candidate config: the exact bytes that will replace
// config.yaml, plus what the apply pipeline needs to describe and verify itself.
type Staged struct {
	// Data is the rendered candidate — raw, with ${VAR} references intact.
	Data []byte
	// Hash is Data's sha256, hex-encoded. The pending-apply marker carries it so
	// a daemon booting after a crash can tell "the write landed" from "it never
	// happened".
	Hash string
	// Summary is the op's one-liner, for the marker and the verdict.
	Summary string
}

var _ Service = (*svc)(nil)

type svc struct {
	configPath  string
	secretsPath string
	now         func() time.Time
}

func New(configPath, secretsPath string) Service {
	return &svc{configPath: configPath, secretsPath: secretsPath, now: time.Now}
}

func (s *svc) ConfigPath() string { return s.configPath }

// Stage works on a raw draft so credentials stay ${VAR}, and resolves against a
// fresh disk read so a secret written seconds ago already counts.
func (s *svc) Stage(ops ...Op) (*Staged, Verdict) {
	if len(ops) == 0 {
		return nil, Reject("", errors.New("nothing to apply"))
	}

	draft, err := s.rawDraft()
	if err != nil {
		return nil, Reject("", err)
	}

	summaries := make([]string, 0, len(ops))

	for _, op := range ops {
		if err := op.apply(draft); err != nil {
			return nil, Reject(op.Path(), err)
		}

		summaries = append(summaries, op.Summary())
	}

	// The whole set is anchored at one path only when it is one op; a set that
	// spans sections has no field to point at.
	path := ""
	if len(ops) == 1 {
		path = ops[0].Path()
	}

	data, err := config.MarshalUnifiedConfig(draft)
	if err != nil {
		return nil, Reject(path, fmt.Errorf("render config: %w", err))
	}

	secrets, err := config.LoadSecretsFrom(s.secretsPath)
	if err != nil {
		return nil, Reject("", err)
	}

	if _, err := config.ParseAndResolve(data, secrets); err != nil {
		return nil, Reject(path, err)
	}

	sum := sha256.Sum256(data)

	return &Staged{
		Data:    data,
		Hash:    hex.EncodeToString(sum[:]),
		Summary: strings.Join(summaries, "; "),
	}, OK()
}

// Commit's write order is the contract — backup, marker, config. The marker's
// hash is what tells the next boot whether the write ever landed.
func (s *svc) Commit(staged *Staged, p Pending) Verdict {
	if staged == nil {
		return Reject("", errors.New("nothing staged"))
	}

	bak, err := backupConfig(s.configPath, s.now().Format(backupStamp))
	if err != nil {
		return Reject("", err)
	}

	p.BakPath = bak
	p.NewHash = staged.Hash

	if p.Summary == "" {
		p.Summary = staged.Summary
	}

	if err := writeMarker(s.markerPath(), p); err != nil {
		return Reject("", err)
	}

	if err := writeConfigFile(s.configPath, staged.Data); err != nil {
		return Reject("", err)
	}

	pruneBackups(s.configPath)

	return OK()
}

// rawDraft loads the config without resolving secrets. A missing file is the
// pre-onboarding state, not an error: the first op writes the file.
func (s *svc) rawDraft() (*config.UnifiedConfig, error) {
	draft, err := config.LoadRawUnifiedConfig(s.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return &config.UnifiedConfig{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("load config draft: %w", err)
	}

	return cloneConfig(draft), nil
}

// cloneConfig deep-copies the mutable parts of a draft, so an op that fails
// halfway cannot leave a partially mutated draft behind.
func cloneConfig(c *config.UnifiedConfig) *config.UnifiedConfig {
	out := *c
	out.Providers = maps.Clone(c.Providers)
	out.Marketplaces = slices.Clone(c.Marketplaces)
	out.Models = slices.Clone(c.Models)
	out.Managers = slices.Clone(c.Managers)
	out.SpawnFavorites = slices.Clone(c.SpawnFavorites)
	out.Tools.Bash.Sandbox.WritablePaths = slices.Clone(c.Tools.Bash.Sandbox.WritablePaths)

	return &out
}
