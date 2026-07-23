# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- Project defaults in `.claude/pigeon.json`: a checkout can declare the `name`,
  `description` and `topics` that sessions started in it come up with, so a team
  shares one wiring by committing a file. The config seeds a session's first
  registration only, leaving `pigeon name`, `describe` and `unsubscribe`
  authoritative afterwards, and it never takes a name another live session already
  holds. `pigeon doctor` reports which config was found and what it applied.

  The file arrives with a `git clone`, so it is treated as untrusted: a name that is
  not a valid address is rejected rather than sanitised, the description is flattened
  and bounded, topics are validated and capped, and the read itself is size-limited.
  A rejected field is dropped and reported rather than failing the whole file.

### Fixed
- Re-registering a session no longer fast-forwards its topic cursors. A monitor
  restart could skip everything published to a subscribed topic while it was down.

## [0.1.0] - 2026-07-23

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
- `pigeon doctor` checks each link in the delivery chain separately -- session id,
  state directory, plugin, monitor binary, MCP registration, this session's
  registration -- and names the one that broke, rather than leaving "messages
  stopped arriving" as the only symptom. Exits non-zero when delivery is broken, so
  it works in a health check; `--json` for scripts. It warns when Claude Code is
  newer than the version the mechanism was verified against, and catches the upgrade
  trap where `go install` writes a new binary while the plugin keeps arming the old
  one.
- `pigeon statusline` renders a one-line alarm for a Claude Code statusline. A
  healthy session renders nothing at all: a live monitor drains the spool within
  about a second, so there is no standing unread count worth showing. The line
  appears only when this session cannot receive -- `deaf`, with the number of
  messages genuinely waiting, or `not armed`.
- `pigeon prune` reclaims topic logs: a log nobody subscribes to is removed, and
  the prefix every live subscriber has already read is compacted away. Followers
  detect a replaced log and resume from their cursor rather than replaying it.
- `pigeon ls` and the `list_sessions` tool say which row is the calling session, and
  say so explicitly when the caller is not registered or is a plain shell.
- Builds for Linux, macOS, FreeBSD, OpenBSD and Windows. File locking and process
  liveness are behind a platform abstraction; on Windows the CLI works but the
  monitor stands down, since Claude Code only arms plugin monitors on Unix.
- GoReleaser configuration and a release workflow; CI cross-compiles every supported
  target and validates the release config.
- `skills/session-coordination`, an example Claude Code skill. Not installed by
  `pigeon install` -- copy it to `~/.claude/skills/` deliberately.
- `pigeon version` reports the commit and build date.
- The generated plugin manifest carries author, homepage and licence, so
  `claude plugin validate` is warning-free.
- A message sent from a plain shell is marked as having no reply address. Saying
  nothing is not enough: recipients read the `shell:user@host` stamp as an address
  and waste a call discovering it is not one.

[Unreleased]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/PeterSR/claude-code-pigeon/releases/tag/v0.1.0
