# Contributing

Thanks for considering it. Issues and pull requests are both welcome.

## Getting started

```console
git clone https://github.com/PeterSR/claude-code-pigeon
cd claude-code-pigeon
make test
```

No dependencies beyond the Go standard library, and the project intends to keep it that
way. If a change needs a third-party module, please open an issue first so we can talk
about whether it earns its place.

## Before opening a pull request

```console
make fmt vet test
```

CI runs the same checks on Linux and macOS, with `-race`.

## Testing conventions

Tests use `t.TempDir()` plus `t.Setenv(EnvHome, …)` so they never touch a real
`~/.claude/pigeon`. Anything that registers a session should use the helpers in
`pigeon_test.go` (`withHome`, `liveEntry`) rather than writing entry files by hand.

Tests must not require a running Claude Code session.

## Things worth knowing before you change the monitor

The monitor is the part with sharp edges. `docs/ALTERNATIVES.md` lists the ones that have
bitten other projects; the short version:

- Identity comes from `CLAUDE_CODE_SESSION_ID`, read **inside the process**. Never
  interpolate it in `monitors.json`.
- If identity is missing, fail loudly. Guessing delivers another session's mail.
- Notifications are clipped at about 512 characters.
- Emitting too much gets the monitor stopped by Claude Code.
- Anything rendered into a notification is untrusted input. Keep it sanitised, and keep it
  phrased as a report rather than an instruction.

## Scope

pigeon is deliberately small: one machine, one binary, no network transport, no auth. If
you want encryption or cross-agent support, `docs/ALTERNATIVES.md` points at projects that
already do those well.

## Reporting security issues

See [SECURITY.md](SECURITY.md).
