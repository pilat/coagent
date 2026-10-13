package configops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/pilat/coagent/internal/config"
)

// ReplaceSummary is the applied receipt's one-liner; it restates the deletion
// semantics so a model cannot read an applied fragment as a merge.
const ReplaceSummary = "replaced the ENTIRE configuration document — every section or entry absent from it was deleted"

// Service is the whole-document mutation layer: config_edit stages a complete
// candidate, the daemon commits it and resolves the pending marker on boot.
type Service interface {
	// StageDocument validates a caller-supplied complete candidate document —
	// strict parse, secrets resolved, defaults and semantic checks applied —
	// and returns the staged candidate without writing. It is the raw-document
	// editing path for config_edit.
	StageDocument(candidate []byte) (*Staged, Verdict)
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

// StageDocument validates the complete candidate as given. Credentials may be
// literal values or ${VAR} references; semantic validation resolves against a
// fresh disk read. The staged bytes are the validated candidate verbatim —
// user formatting is preserved and the hash matches what a boot will parse.
func (s *svc) StageDocument(candidate []byte) (*Staged, Verdict) {
	if len(bytes.TrimSpace(candidate)) == 0 {
		return nil, Reject("", errors.New("empty configuration document"))
	}

	parsed, err := config.ParseUnifiedConfig(candidate)
	if err != nil {
		return nil, Reject("", err)
	}

	if len(parsed.Models) == 0 {
		return nil, Reject(
			"",
			errors.New(
				"complete replacement configuration must include at least one model; configuration fragments cannot replace the live document",
			),
		)
	}

	secrets, err := config.LoadSecretsFrom(s.secretsPath)
	if err != nil {
		return nil, Reject("", err)
	}

	if _, err := config.ParseAndResolve(candidate, secrets); err != nil {
		return nil, Reject("", err)
	}

	sum := sha256.Sum256(candidate)

	return &Staged{
		Data:    candidate,
		Hash:    hex.EncodeToString(sum[:]),
		Summary: ReplaceSummary,
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
