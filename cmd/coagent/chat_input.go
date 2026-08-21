package main

import (
	"context"
	"errors"
	"io"
	"strings"
)

// readLines is the single owner of the terminal's read side — a masked prompt is a
// mode of this loop, never a second reader racing it for a credential.
func (c *chat) readLines(ctx context.Context) int {
	c.prompt()

	for {
		if err := c.takeFatal(); err != nil {
			c.errorf("%v", err)

			return exitError
		}

		if req, ok := c.takeSecret(); ok {
			if err := c.askForSecret(ctx, req, ""); err != nil {
				c.errorf("%v", err)

				return exitError
			}

			continue
		}

		line, err := c.term.ReadLine()

		switch {
		case errors.Is(err, errNoInput):
			continue
		case errors.Is(err, io.EOF):
			return exitOK
		case err != nil:
			c.errorf("read: %v", err)

			return exitError
		}

		if err := c.consume(ctx, strings.TrimSpace(line)); err != nil {
			if errors.Is(err, io.EOF) {
				return exitOK
			}

			c.errorf("%v", err)

			return exitError
		}
	}
}

// consume routes one completed line. A secret request that landed mid-typing owns
// that line: the value goes to the daemon, never into the conversation.
func (c *chat) consume(ctx context.Context, line string) error {
	if req, ok := c.takeSecret(); ok {
		return c.askForSecret(ctx, req, line)
	}

	if line == "" {
		c.prompt()

		return nil
	}

	if line == "/stop" {
		c.stopTurn(ctx)

		return nil
	}

	if line == "/model" {
		return c.chooseModel(ctx)
	}

	return c.send(ctx, line)
}

func (c *chat) prompt() {
	if !c.isBusy() {
		c.write("> ")
	}
}
