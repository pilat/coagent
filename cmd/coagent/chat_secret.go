package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/managers/cli"
)

// errSecretDismissed ends a masked prompt whose request somebody else answered.
// Whatever was being typed at it is dropped, never sent anywhere.
var errSecretDismissed = errors.New("the secret request was resolved elsewhere")

// takeSecret claims the next unanswered masked prompt, skipping any request that
// was resolved elsewhere while it waited its turn.
func (c *chat) takeSecret() (cli.SecretRequest, bool) {
	for {
		select {
		case req := <-c.secrets:
			if c.claimDismissed(req.RequestID) {
				continue
			}

			return req, true
		default:
			return cli.SecretRequest{}, false
		}
	}
}

// askForSecret sends one credential straight to the daemon, never through the chat
// stream: it is stored by name, and the model only ever learns the name.
func (c *chat) askForSecret(ctx context.Context, req cli.SecretRequest, typed string) {
	if req.Purpose != "" {
		c.println(req.Purpose)
	}

	value, err := c.secretValue(req, typed)

	switch {
	case errors.Is(err, errSecretDismissed):
		c.printf("%s: provided in another terminal\n", req.Name)
		c.prompt()

		return
	case err != nil:
		c.errorf("could not read the value: %v", err)

		return
	}

	// Claimed before the call: the daemon's dismissal races the response back, and
	// this terminal's own answer must not read as news from somewhere else.
	c.noteAnswered(req.RequestID)

	if value == "" {
		c.declineSecret(ctx, req)

		return
	}

	err = c.call(ctx, ctl.OpSetSecret, ctl.SetSecretParams{
		Name:      req.Name,
		Value:     value,
		RequestID: req.RequestID,
	}, nil)
	if err != nil {
		c.errorf("%v", err)
	}
}

// declineSecret answers the prompt with a refusal, so the session stops waiting
// on somebody who is not going to type it. Nothing is stored.
func (c *chat) declineSecret(ctx context.Context, req cli.SecretRequest) {
	c.printf("declined %s\n", req.Name)

	err := c.call(ctx, cli.OpChatSecretCancel, cli.SecretCancelParams{
		SessionID: c.currentSession(),
		RequestID: req.RequestID,
	}, nil)
	if err != nil {
		c.errorf("%v", err)
	}
}

// secretValue reads the credential, or adopts the line the user had already
// typed when the request arrived — that line was never sent anywhere.
func (c *chat) secretValue(req cli.SecretRequest, typed string) (string, error) {
	if typed != "" {
		c.printf("%s: took the line you just typed, it was not sent to the chat\n", req.Name)

		return typed, nil
	}

	c.printf("%s (hidden, empty line to decline): ", req.Name)

	value, err := c.readMasked(req)

	c.println("")

	if err != nil {
		return "", err
	}

	return value, nil
}

// readMasked polls the masked read so the loop keeps regaining control between
// keystrokes: a request answered in another terminal has to close this prompt
// rather than wait for a value nobody is going to type here.
func (c *chat) readMasked(req cli.SecretRequest) (string, error) {
	for {
		if c.claimDismissed(req.RequestID) {
			c.term.EndSecret()

			return "", errSecretDismissed
		}

		value, err := c.term.ReadSecret()
		if errors.Is(err, errNoInput) {
			continue
		}

		return value, err
	}
}

// dismissSecret records that a request is over. The daemon tells every attached
// terminal, including the one that answered, for which it is an echo not news.
func (c *chat) dismissSecret(params json.RawMessage) {
	var res cli.SecretResolved
	if err := json.Unmarshal(params, &res); err != nil {
		return
	}

	c.secretMu.Lock()
	defer c.secretMu.Unlock()

	if c.answered[res.RequestID] {
		delete(c.answered, res.RequestID)

		return
	}

	c.dismissed[res.RequestID] = true
}

// claimDismissed reports and clears a pending dismissal for one request.
func (c *chat) claimDismissed(requestID string) bool {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()

	if !c.dismissed[requestID] {
		return false
	}

	delete(c.dismissed, requestID)

	return true
}

func (c *chat) noteAnswered(requestID string) {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()

	c.answered[requestID] = true
}
