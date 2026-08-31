package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/llm"
)

// replyHook is the half of a control connection a config op needs: run something
// once the answer is on the wire, and learn that the connection is gone.
type replyHook interface {
	AfterReply(fn func())
	Done() <-chan struct{}
}

// registerConfigOps wires the bootstrap ops onto the control socket. They are
// the deterministic half of onboarding: everything past the first provider key
// happens in chat, through the session config tools.
func registerConfigOps(srv *ctl.Server, applier configapply.Service, resolver secretRequestResolver) error {
	setProviderHandler := func(_ context.Context, c *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p ctl.SetProviderParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		return setProvider(applier, c, p), nil
	}
	if err := registerControlHandler(srv, ctl.OpSetProvider, setProviderHandler); err != nil {
		return err
	}

	// The sudo-free update's second half: the binary is already swapped, so the
	// exec picks it up. No config change, hence no verdict and no marker.
	restartHandler := func(_ context.Context, c *ctl.Conn, _ json.RawMessage) (any, *ctl.Error) {
		c.AfterReply(applier.Restart)

		return ctl.RestartResult{Restarting: true}, nil
	}
	if err := registerControlHandler(srv, ctl.OpRestartDaemon, restartHandler); err != nil {
		return err
	}

	setSecretHandler := func(ctx context.Context, c *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p ctl.SetSecretParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		return setSecret(ctx, applier, resolver, c, p), nil
	}

	return registerControlHandler(srv, ctl.OpSetSecret, setSecretHandler)
}

func registerControlHandler(server *ctl.Server, op string, handler ctl.Handler) error {
	if err := server.Register(op, handler); err != nil {
		return fmt.Errorf("register %s: %w", op, err)
	}

	return nil
}

// setProvider claims the apply slot before it touches anything: the slot is
// daemon-wide (a session may already be suspended on a change of its own), and a
// caller refused after the credential write would leave an orphan on disk.
func setProvider(applier configapply.Service, c replyHook, p ctl.SetProviderParams) configops.Verdict {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Driver) == "" {
		return configops.Reject("providers", errors.New("a provider needs a name and a driver"))
	}

	if !applier.ClaimApply() {
		return configops.Reject("", errors.New("another config change is being applied — try again after the restart"))
	}

	if v := commitProvider(applier, p); v.Failed() {
		applier.ReleaseApply()

		return v
	}

	restartOnCommit(c, applier)

	return configops.OK()
}

// restartOnCommit brings the daemon back on what was just written. The answer
// goes out first when it can, but a reply that never reaches the wire skips the
// hook — and a committed change whose restart never runs leaves the daemon on
// the superseded config, still holding the apply slot, for the rest of its life.
func restartOnCommit(c replyHook, applier configapply.Service) {
	c.AfterReply(applier.Restart)

	go func() {
		<-c.Done()

		applier.Restart()
	}()
}

// commitProvider writes the credential first and the config that references it
// second. The order is the invariant: a crash or a refusal between the two leaves
// an orphan secret — inert, and overwritten by the retry — never a config
// pointing at a ${VAR} that does not exist, which is fatal at the next boot.
func commitProvider(applier configapply.Service, p ctl.SetProviderParams) configops.Verdict {
	ops := applier.Ops()

	entry := config.ProviderEntry{
		Driver:  p.Driver,
		SAFile:  p.SAFile,
		BaseURL: p.BaseURL,
		Catalog: p.Catalog,
	}

	if p.APIKey != "" {
		v := configops.SecretVarForProvider(p.Name)
		if _, sv := ops.SetSecret(v, p.APIKey); sv.Failed() {
			return sv
		}

		entry.APIKey = configops.Ref(v)
	}

	mutations := []configops.Op{configops.SetProvider(p.Name, entry)}
	for _, id := range providerModels(entry, p.Models) {
		mutations = append(mutations, configops.AddModel(config.ModelEntry{ID: id, Provider: p.Name}))
	}

	staged, v := ops.Stage(mutations...)
	if v.Failed() {
		return v
	}

	// Commit before answering: a bootstrap caller has no session to receive a
	// later verdict, so the RPC result is the whole answer. The restart then runs
	// once that answer is on the wire — the marker carries the rollback if the
	// daemon cannot come back on the new file.
	return ops.Commit(staged, configops.Pending{})
}

// providerModels decides which models land with a new provider. An explicit list
// wins; otherwise the vendor's recommendation fills it in, because a provider
// with no model is a config that cannot start a session — and the chat that
// would fix it is the very thing that needs one.
func providerModels(entry config.ProviderEntry, explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}

	section, ok := llm.CatalogSection(entry)
	if !ok {
		return nil
	}

	rec, ok := recommend(section)
	if !ok {
		return nil
	}

	return rec.models()
}

// setSecret stores one credential. A variable the config already references is a
// rotation: the file did not move, but the running process is holding the old
// value, so it has to come back to pick up the new one.
func setSecret(
	ctx context.Context,
	applier configapply.Service,
	resolver secretRequestResolver,
	c replyHook,
	p ctl.SetSecretParams,
) configops.Verdict {
	// Claim the prompt before writing: it is re-pushed to every terminal that
	// attaches, so a refused answer must not clobber the one that won.
	if p.RequestID != "" {
		if err := resolver.ResolveSecretRequest(ctx, p.RequestID, p.Name); err != nil {
			return configops.Reject("", fmt.Errorf("%w — %s was not stored", err, p.Name))
		}
	}

	referenced, v := applier.Ops().SetSecret(p.Name, p.Value)
	if v.Failed() {
		return v
	}

	if referenced {
		restartOnCommit(c, applier)
	}

	return v
}
