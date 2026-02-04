# coagent

A headless coding agent that runs as a daemon.

The idea is simple: you hand it a task and walk away. There is no TUI, no editor
plugin and no web UI — the agent takes work from somewhere outside the terminal,
runs a plan/act loop with tools until it has an answer, and reports back. Every
interactive assistant I have used assumes a human is sitting there steering it.
This one assumes nobody is.

Very early. Nothing here is stable yet.

## Build

Requires Go 1.25 (pinned in `mise.toml`).

```bash
mise install
make build      # -> ./coagent
make test
```

## Layout

```
cmd/coagent/    binary entrypoint
internal/       everything else
```

## Configuration

Config lives under `~/.coagent`. API keys go in `~/.coagent/secrets` — see
`secrets.example`. Nothing is read from the current working directory.

## License

Not decided yet.
