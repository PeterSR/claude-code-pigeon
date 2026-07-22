# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-07-22

First release.

### Added
- Direct messages between live Claude Code sessions, delivered to idle sessions in about a
  second with no polling and no manual arming.
- Pub/sub topics with per-subscriber cursors; every session joins `all` by default.
  Subscription changes take effect in a running monitor without a restart.
- Session registry with self-declared name and description, addressable by session id,
  name, id prefix, or working-directory basename.
- Monitor liveness reporting: `live`, `deaf` (session running, nothing listening) and
  `dead`, detected via an flock the kernel releases on process exit.
- MCP server exposing `list_sessions`, `send_message`, `publish`, `subscribe`,
  `unsubscribe`, `list_topics`, `whoami`, `set_identity`.
- `pigeon install` writes its own plugin; `pigeon arm` covers per-session arming without
  one.
- Opt-out via `PIGEON=0` for programmatically driven sessions.
- Overflow bodies spill to a payload file so notifications stay inside Claude Code's
  ~512-character clip.

[Unreleased]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/PeterSR/claude-code-pigeon/releases/tag/v0.1.0
