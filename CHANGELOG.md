# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- `pigeon listen` receives messages in a plain shell, so automation outside Claude Code can
  subscribe to topics and react. It blocks, printing each message as it arrives, reusing the
  monitor's own follow loop, so it inherits the same compaction- and truncation-tolerance.
  With one or more topics it is an anonymous tail; at a terminal it prints the human line a
  session would see, and piped it emits one JSON object per line (`--json`/`--plain` force
  either). `--replay` drains history rather than starting from now, `--count N` stops after N
  messages, and `--timeout <dur>` after a while.

  Given an identity it becomes a visible **inbox**: it registers an ephemeral session, shows
  up in `pigeon ls` as a `shell`, is addressable as `pigeon send <name>` like any peer, and
  vanishes when the shell exits.
- An **acting identity** for shells, spelled three ways like `pigeon namespace`: a standing
  `pigeon as <name>` kept in `~/.claude/pigeon/cli.json`, a `--as <name>` flag on
  `listen`/`send`/`publish`, and a `PIGEON_AS` environment variable. Highest wins: `--as`,
  then a real Claude Code session, then `PIGEON_AS`, then `pigeon as`, then a plain shell.
  A shell with an identity stamps its `send` and `publish` as that inbox, so replies route
  back -- but only while the inbox is actually listening; otherwise the post falls back to a
  plain `shell:user@host` with no reply address, so a standing identity is never a promise
  nothing can keep. `list_sessions` and `pigeon ls` mark such rows so a shell inbox is not
  mistaken for a Claude Code session.
- The pid is now a send target. `pigeon send <pid> "..."` resolves the session owning that
  claude process, ahead of the fuzzier name, prefix and cwd tiers, since a pid is exact and
  unique among live sessions. Useful when a session is unnamed and you already have its pid.
- Claude Code's own session name -- the one `/status` shows -- is surfaced as a new `CLAUDE`
  column in `pigeon ls`, in `list_sessions` and `whoami`, and as a `{{.ClaudeName}}`
  (and `{{.ClaudeNameSource}}`) template field for project configs. It is a host label, not
  an address: routing still keys on the pigeon `name`, and the Claude name is not a send
  target. A derived name mostly echoes the cwd; it earns its place when a session is renamed
  in Claude Code, which pigeon cannot derive on its own.

  It is read best-effort from Claude Code's per-session index
  (`~/.claude/sessions/<pid>.json`, relocated by `CLAUDE_CONFIG_DIR`), keyed by the claude
  pid and verified against the session id before it is trusted, so a recycled pid cannot
  mislabel a session. Like every other undocumented host behaviour pigeon leans on, `pigeon
  doctor` checks it and it degrades to empty rather than failing. A private session withholds
  it along with its cwd, because a derived name would leak the directory.
- `pigeon ls` also shows a `PID` column, and `whoami` a `pid:` line.
- `pigeon weaverbird spec|value`, which publishes pigeon's state to
  [weaverbird](https://github.com/PeterSR/claude-code-weaverbird) rather than rendering a
  status line itself. Same alarm as before, spoken to a host that owns the bar: `pigeon.wait`
  is silent for a session that can receive, and says `not armed` or `N waiting` when it
  cannot. Two opt-in widgets go further for a layout that asks for them by id --
  `pigeon.monitor` (`live`/`deaf`/`dead`) and `pigeon.peers` (other live sessions in this
  namespace) -- grouped as `pigeon.detail` so asking is one id instead of three.

  `pigeon.wait` declares a 30s cache ceiling plus a file invalidation on this session's own
  spool, so a message arriving relights the widget without waiting out the ttl.

### Removed
- `pigeon statusline`. weaverbird renders pigeon's status now, and two commands answering the
  same question is how they drift apart. If you had it wired into `statusLine` in
  `settings.json`, point that at weaverbird and register pigeon as a provider; the README says
  how. Nothing else changes: the widget reports the same states from the same registry.

### Fixed
- A session that has been cleared is no longer reported as `not armed`. Clearing mints a fresh
  session id inside the running claude process, but the monitor was spawned once when that
  process started and keeps the id it armed with, along with the registry entry, spool and lock
  it owns. The widget is handed the host's current id, finds nothing filed under it, and cannot
  fall back on the arming grace either, since Claude Code keeps the original `startedAt` across
  the change -- so a session that was alive and draining its mail read as never armed. A session
  id that finds no entry is now also looked up by the claude process behind it (pid and process
  start time, which name one process exactly), and the entry its monitor armed with is found.
- A freshly started session is no longer reported as `not armed`. A host renders its status
  line at session start and caches it, re-running the provider only on a real turn, so the
  first render often lands before the monitor has registered and the false alarm then sticks
  on an idle session's bar. A missing entry is now silent for the session's first few seconds
  (`startedAt` from Claude Code's session index), during which the monitor is still arming;
  past that window a missing entry is real and reported as before.

## [0.2.0] - 2026-07-23

### Added
- Namespaces: isolated groups of sessions, with `default` for anyone who never thinks
  about them. A session sees, resolves, names and broadcasts only inside its own, so two
  namespaces can both hold a session called `api` without either losing its address.

  The isolation is structural rather than a filter: each namespace owns a complete state
  tree, so a session in `acme` cannot see `default`'s registry because that is not the
  directory it reads. A field plus a filter would have put the rule in `ListSessions`,
  target resolution, name uniqueness, topic listing, the publish subscriber count, prune,
  doctor and every MCP tool, and missing one of them leaks somebody else's sessions.

  A namespace comes from `PIGEON_NAMESPACE`, then `"namespace"` in the project's
  `.claude/pigeon.json`, then `pigeon namespace <name>`, then `default`. It is fixed when
  a session's monitor arms, for the same reason monitors cannot be rebound mid-session:
  the monitor holds a lock in that namespace's directory and follows its topics. Moving a
  session means restarting it.
- Global topics: a topic written `@ops` is one log the whole machine shares, while `ops`
  is one log per namespace. The `@` works everywhere a topic is accepted -- CLI, MCP and
  the project config -- and is shell-safe, unlike `*`, `!` or `~`. Every session now
  subscribes to `@all` as well as `all`, deliberately: a broadcast meant for everyone on
  the machine has to reach everyone on the machine. `pigeon unsubscribe @all` opts out.
- `pigeon namespaces` lists every namespace with its live and deaf session counts, and
  `pigeon namespace [<name>]` shows or sets the one shell invocations use, recorded in
  `<state>/cli.json`. Setting it is `kubectl config set-context`, not a live move, and it
  says so; it also says when something already outranks the preference just set.
- `-n`/`--namespace` on `ls`, `send`, `publish`, `topics` and `prune`, and
  `--all-namespaces` on `ls`, `topics` and `prune`. Cross-namespace `send` is allowed:
  anyone who can write the state directory could append to that spool by hand, so blocking
  it would buy inconvenience rather than isolation. `pigeon prune --all-namespaces` is
  what clears the entry a session leaves behind in its old namespace when a project config
  changes which one it declares.
- `pigeon ls` ends with a count of the sessions it is not showing, and only when there are
  some. Isolation you have forgotten about looks exactly like an empty machine, so the
  footer is what makes the mechanism discoverable again. `--all-namespaces` gains a
  NAMESPACE column, and `--json` and `list_sessions` publish the namespace so consumers do
  not have to infer it.
- A notification names the sender's namespace, and qualifies its reply hint with `-n`,
  exactly when the message could have arrived from outside the recipient's namespace: a
  global topic, or a cross-namespace direct message. Everywhere else it is a constant, and
  a constant in every notification is noise. The namespace is sanitised and bounded like
  every other peer-controlled field, and the whole line still fits the render budget.
- The MCP server gains `list_namespaces`, and an optional `namespace` input on
  `list_sessions`, `send_message`, `publish`, `subscribe` and `unsubscribe`. A namespace
  that cannot be a directory name is refused rather than replaced, since acting on
  `default` instead of what was asked for is how a message reaches the wrong people.
- `pigeon doctor` reports the namespace and where it came from, and its peers check counts
  what is deliberately out of sight. A session that quietly landed in the wrong namespace
  registers fine, sends fine, and simply cannot see anyone, so "where did this come from"
  is the whole diagnosis.
- `pigeon statusline` is unchanged and says nothing about namespaces: silent when healthy,
  `deaf` or `not armed` otherwise. It does now look a session up in every namespace before
  calling it unarmed, because it is spawned per render with an environment and a working
  directory that need not match the ones its monitor armed with, and one wrong lit line is
  what turns the widget into wallpaper.
- Private namespaces, declared in a machine-level config at
  `$XDG_CONFIG_HOME/pigeon/config.json`. A private namespace is invisible and
  unaddressable from inside a Claude Code session, entirely normal from within
  itself, and sealed against machine-wide `@` topics in both directions.

  Policy is deliberately not a project setting: a `.claude/pigeon.json` arrives
  with a `git clone`, so a repository may say which namespace its sessions join
  but never that a namespace is private. The rule keys on
  `CLAUDE_CODE_SESSION_ID`, which covers the MCP server and an agent's shell
  alike while leaving your own terminal as the escape hatch. It is a boundary
  against ordinary reach rather than a sandbox, and the README says so.
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
- `name`, `description` and the new `onNameTaken` in `.claude/pigeon.json` are Go
  `text/template` source, rendered per session, so a checkout can declare what its
  sessions should be called rather than what one of them is called. A string with no
  `{{` is still a literal. The context is `.Cwd`, `.Dir`, `.Branch` (read straight from
  `.git/HEAD`, handling a detached HEAD and a `.git` file), `.Host`, `.User`,
  `.Session`, `.Short` and `.Seq`, plus `snake`, `kebab`, `lower`, `upper`, `trunc` and
  `default`. `onNameTaken` is tried exactly once when the rendered name is already held
  by a live session, so `{{.Name}}-{{.Seq}}` brings the second session in a checkout up
  as `api-2`; a fallback that is also taken leaves the session unnamed and says why,
  rather than hunting for a free name nobody declared.

  The templates get the same treatment as the rest of the file, because a name is
  rendered into other sessions' notification lines: the source is length-bounded before
  parsing, execution writes into a bounded buffer so no template can produce unbounded
  output, and a rendered name is validated and rejected rather than repaired. A broken
  template is a reported problem, never a failed registration.
- `pigeon name --template` and `pigeon describe --template`, and `nameTemplate` /
  `descriptionTemplate` on the MCP `set_identity` tool, rendering the same context. A
  rendered name that another live session holds is refused, and the message names the
  value that collided rather than the template.
- `"enabled": false` in a project config keeps sessions started in that checkout off the
  bus entirely, as `PIGEON=0` does for one session. The environment outranks the file in
  both directions.
- `"private": true` registers the session and leaves it addressable, but publishes no
  cwd and no description, so neither reaches another session's listing or notification
  lines. It is enforced on every write to the registry entry rather than only at
  startup, and a private session's outgoing messages carry no directory either.
- `pigeon doctor` reports the project config's *rendered* values, since with templates
  the file no longer contains them, along with any template problem, whether the project
  is private, and whether it has taken itself off the bus.

### Changed
- **Breaking (on-disk layout).** The six state directories moved from `<state>/sessions`,
  `inbox`, `topics`, `cursors`, `locks` and `payloads` to
  `<state>/namespaces/<namespace>/…`, with a new `<state>/shared/` holding the
  machine-wide topic logs and their payloads. The first pigeon command after the upgrade
  moves the old tree into `namespaces/default/`, once, under a lock, idempotently, and
  logs that it did: live sessions keep their queued mail, their cursors and their
  addresses. Anything that read those paths directly has to change; nothing else does.

### Fixed
- Re-registering a session no longer fast-forwards its topic cursors. A monitor
  restart could skip everything published to a subscribed topic while it was down.
- A cursor is a logical offset, bytes since the beginning of a log's life, and each log
  records how much compaction has thrown away. It used to be a raw file position that
  compaction rewound behind the followers' backs, so between the rewrite and the rewind
  the file and the cursor described different eras of the same log: a follower that read
  in that window either skipped the whole compacted log permanently, around 350 messages
  in the reproduction, or replayed it and had the rate limiter drop 319 of them.
  Compaction now adds to the log's base and touches no cursor at all.
- `pigeon prune` no longer sweeps lock files. It trimmed the suffix and read the
  remainder as a session id, so `<sid>.entry.lock` and `topic-deploys.lock` became names
  nobody had registered and were removed, for live sessions and active topics alike.
  Unlinking a lock is how two processes come to hold different inodes of it: with a
  publisher holding a topic lock, a single `pigeon prune` removed the lock, the
  compaction later in the same command took a fresh inode instead of blocking, and a line
  already reported as sent went to the replaced log. An abandoned lock is a zero-byte
  inode and costs nothing to keep.
- `pigeon prune` reclaims payload files. A body that overflows the notification budget
  spills to a file, and nothing removed it afterwards except `uninstall --purge`. Prune
  keeps exactly the payloads still named by a message in a surviving spool or topic log,
  which is exact where an age rule would not be: an unread message on a deaf session's
  spool needs its payload however old it is, and one whose message has been compacted
  away is garbage the moment it goes.
- A notification never trims its payload pointer, which is the only route to a body that
  did not fit. When head and tail exceeded the budget the body's allowance was clamped
  upward and a final truncate cut the tail, where the pointer sits. Hints are now dropped
  whole and cheapest first: the topic, which the header already names, then the working
  directory and the reply address, both recoverable with `pigeon ls`, then the namespace
  tag. The header is built from parts that can be given up too, so a long state path
  cannot push the line over the budget with nothing to catch it.
- A zero render budget no longer panics the monitor by indexing backwards.
- The MCP server no longer spins on malformed input. It answered a parse error and
  carried on, but a `json.Decoder` does not resync after a syntax error, so the bad bytes
  stayed buffered and every later decode failed on the same input: it never served
  another request and never exited. The framing is newline-delimited, so it now reads a
  line at a time, which consumes the bad input.
- The rate limiter's notice names the log its suppressed messages went to. It named the
  direct spool for everything, so a suppressed topic message sent the recipient to a file
  it had never been written to, and that notice is the only recovery hint they get.
  Anything still suppressed when the monitor stops is now reported rather than dropped.

### Security
- `Sanitize` folds square brackets and drops the Unicode formatting categories. Every
  hint in a notification is bracketed and only angle brackets were neutralised, so an
  ordinary message body could forge a payload pointer at any path, a reply address it did
  not own, or an entire second notification from a peer that never sent one.
  `unicode.IsControl` reports only Latin-1, so a bidi override went through as well.

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

[Unreleased]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/PeterSR/claude-code-pigeon/releases/tag/v0.1.0
