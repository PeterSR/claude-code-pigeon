# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- **Session registration is a plugin hook, so the monitor is genuinely optional.**
  `SessionStart` runs `pigeon register` and `SessionEnd` runs `pigeon deregister`, both
  written by `pigeon install` and both new commands. Registration used to be something the
  monitor did on its way to delivering, which is what made the process mandatory: no
  monitor meant no entry, and an entry is what carries a session's pid, start token and
  namespace, so a session without one is unreachable by every transport including the
  socket. A hook writes the entry and exits, and unlike a monitor an exiting hook is not
  reported back into the session as a failure.

  This also fixes registration across a resume. `SessionStart` matches `resume` as well as
  `startup`, so a resumed session registers every time; the monitor did not, because Claude
  Code rearms it inconsistently across a resume.

  Neither hook writes to stdout, deliberately: a `SessionStart` hook's stdout is fed back
  into the session as context, so anything printed there would cost tokens in every session.

- `pigeon monitoring [on|off]` decides whether this machine's sessions announce their mail,
  stored as `"monitor"` in the machine config with `PIGEON_MONITOR` overriding it for one
  session either way -- one session opting out of a machine that announces, or opting in on
  a machine that does not.

  A session with no monitor keeps everything except being told. It registers, stays
  addressable, still accumulates mail on its spool and still reads it with `pigeon inbox`;
  it reports as `socket`. What it gives up is the notification: no announcement when
  something arrives, no `digest` or `quiet` (both need a monitor to buffer), and no rate
  limiter, which lives in the monitor too.

  `pigeon doctor` tells a monitor that was turned off apart from one that died, since the
  recovery differs, and fails only on the combination that leaves nothing able to reach the
  session at all: monitoring off and an unreachable socket. It also warns when the installed
  manifest still arms a monitor while the setting says off, which is what an upgrade leaves
  behind. `pigeon arm` says so up front and prints the env override, so arming one session
  by hand still works on a machine that has turned monitoring off.

### Changed
- **No monitor is armed by default.** Installing a plugin is not a decision to run a
  background process in every session from then on, and the socket transport removed the
  reason it had to be one: a running session is reachable without a monitor, so what a
  monitor adds is the *announcing*, which is now something you ask for with
  `pigeon monitoring on`. A machine that has never set it gets sessions that register, stay
  addressable, accumulate mail and read it with `pigeon inbox`, and are not interrupted.

  Off means no process, not a process that starts and stands down. `pigeon monitoring
  on|off` rewrites the plugin's `monitors.json`, listing the monitor or listing nothing,
  because the manifest cannot express the condition itself: `when` accepts only `always` or
  an on-skill-invoke trigger, and a command referencing `${user_config.*}` is rejected at
  plugin load. The hooks are written either way. `pigeon install` preserves the setting
  rather than re-arming.

  **Upgrading: run `pigeon install` once.** Upgrading the binary alone does not rewrite a
  manifest that is already on disk, so an existing install keeps its `when: always` entry
  while the new default reads as off -- which means a monitor that starts, stands down and
  parks in every session: a process per session that announces nothing, the exact
  combination this change exists to remove. `pigeon install` rewrites the manifest to match
  the setting and writes the hooks that older installs do not have. `pigeon doctor` warns
  when it finds that mismatch. If you would rather keep announcing, run
  `pigeon monitoring on`, which rewrites the manifest as well.

### Fixed
- **A monitor exiting no longer unregisters a session that is still running.** Removing the
  entry on the way out was left over from when the monitor was what wrote it; registration
  moved to the `SessionStart` hook and the deferred removal did not move with it, so the
  monitor went on deleting something it no longer owned. The entry names a live claude
  process, so losing it mid-life takes away the address of a session that is still working
  and still answering on its socket, and nothing puts it back: `SessionStart` matches
  `startup` and `resume`, neither of which a running session reaches. Turning monitoring
  off is enough to trigger it for every armed session at once, and a plugin reload, the
  watchdog, a signal or a crash does the same thing one session at a time. Removal belongs
  to `SessionEnd`, which is the only event that means the session is over. Nothing leaks by
  leaving the entry: `register` sweeps entries whose process is gone, and `reconcileOrphans`
  collects the spool and cursors behind them, on the schedule they always did.
- The hook tests had never actually run on Windows, and five of them failed the first time
  they did. `vet` was failing earlier in the job on a test helper that lived in a Unix-only
  file, and once that moved, the hook payload fixture turned out to paste the session's cwd
  between two quotes. A real cwd comes from `t.TempDir()`, which on Windows is a path full
  of backslashes, and between quotes those are JSON escapes -- `\U`, `\A` and `\T` are not
  valid ones -- so the payload never parsed. `readHookInput` treats an unparseable payload
  as an absent one by design, so the hook correctly registered nothing and said it had no
  session id. The fixture is marshalled now, and the regression test feeds a backslashed
  cwd on every platform, because the defect is in reading the payload rather than in
  anything the operating system does.
- **The plugin is installed where Claude Code actually looks for it.** The install target
  was resolved from the home directory alone, but it sits inside Claude Code's own config
  directory, which moves with `CLAUDE_CONFIG_DIR`. On a machine that sets it, `pigeon
  install` scaffolded under `~/.claude/skills`, reported success, and nothing ever loaded
  the plugin; `pigeon monitoring on|off`, which rewrites the manifest in place, wrote to
  that same unread path. It resolves through the same lookup as the rest of Claude Code's
  files now. This also makes the setting testable: exercising `monitoring on|off` used to
  mean writing to the real manifest no matter how the environment was redirected.
- An ask test that passed by luck. `Ask` returns the moment quorum is reached, and the test
  for a session replacing its own answer had a single audience member, so the first answer
  ended the wait and the replacement raced a poller that was already satisfied. It held on
  Linux and macOS and failed on Windows. A second audience member means quorum cannot
  complete until after the change of mind is on file.
- A data race between the two stand-down tests, over the poll interval a parked monitor
  reads. A stood-down monitor idles until its claude process exits, so a test that parks
  one on the test binary's own pid can never end it, and that goroutine keeps reading the
  interval for the rest of the run; the next test shortened it to keep itself quick. It is
  shortened in `TestMain` now, before anything is reading.
- The test suite no longer takes its answers, or its aim, from the machine it runs on. Two
  things escaped `PIGEON_HOME` isolation. The machine config lives under `XDG_CONFIG_HOME`,
  so a developer who had run `pigeon monitoring off` watched twenty delivery tests time out
  as every test monitor read their preference and stood down exactly as asked. Worse, the
  socket transport addresses an operating system process rather than the state directory:
  a test that registers a session stamped the real `CLAUDE_PID` it inherited from `go test`
  run inside a Claude Code session, so a fixture published to a fixture topic resolved a
  live inbox socket and arrived in the developer's own conversation as
  `[pigeon #deploys] from sh :: shipped`. Both channels are isolated now.

## [0.3.0] - 2026-08-18

### Upgrading

The socket transport is on by default, and the suppression that stops a message arriving
twice lives in the RECIPIENT's monitor. So during the window where you have upgraded pigeon
but a session is still running the monitor it armed from an older build, a socket-delivered
message arrives twice: once pushed to the socket, once announced by a monitor that does not
yet know to stay quiet. It is the safe direction to fail in, and it clears the moment a
session restarts. To skip the window entirely, restart your sessions after `pigeon install`,
or set `"transport": "monitor"` in the machine config until they have turned over.

### Added
- A second delivery transport: pigeon can now wake a session by pushing a line to Claude
  Code's own inbox socket, instead of relying on that session's background monitor to find
  it on the spool. `--via monitor|socket|auto` on `send` and `publish`, `PIGEON_TRANSPORT`,
  or `"transport"` in the machine config; `auto` is the default and uses the socket when
  the session can be reached that way. This is deliberately not a project-config field, on
  the same reasoning that keeps `private` out of one: a file that arrives with a `git clone`
  does not get to decide how your sessions are interrupted.

  This exists for the failure mode the README has documented and been unable to do anything
  about. Monitors are armed once, by Claude Code, at session start; nothing respawns one
  that dies, so a session that resumes or merely idles can go deaf silently and mail piles
  up on a spool nothing reads. That session is still *running*, and a running session still
  has its inbox socket bound, so it can still be reached. `pigeon ls` grows a `socket`
  status for exactly that state, `doctor` reports the two paths separately because they
  fail for unrelated reasons, and the status line's `pigeon.wait` alarm no longer fires for
  a session that is receiving perfectly well.

  The spool stays the record. Only the doorbell changes: a socket-delivered message reads
  back identically in `inbox`, threads the same, and is superseded the same. A message
  carries the session ids it was pushed to, and a monitor seeing itself listed advances its
  cursor without announcing the message, so a recipient reachable both ways is not
  interrupted twice. Note that suppression lives in the monitor, so during any period where
  senders have this build and receivers are still running an older monitor, socket-delivered
  messages arrive twice -- the safe direction, but not free.

  `digest` and `quiet` subscribers are never pushed to, whatever `--via` says. A push is
  irrevocable and a sender cannot batch a minute of several senders' traffic into one line,
  which is the whole of what those modes buy; `--supersedes` works by pulling a message out
  of a digest buffer before it fires, and a pushed message is already in front of the
  reader. The consequence worth stating: retiring the monitor would retire those two modes
  with it.

  Two behaviours move. pigeon's 30-per-minute rate limiter lives in the monitor, so a socket
  push is governed by Claude Code's inbound controls instead, which pigeon cannot see. And a
  session that refuses cross-session inbound gets its mail in `inbox` rather than as an
  interruption -- pigeon cannot learn that synchronously, so a push is recorded
  optimistically. Nothing is lost either way; the record is written first.

  The join between pigeon's registry and Claude Code's is on `(pid, procStart)`, not on the
  session id, and that is load-bearing rather than incidental. `/clear` mints a fresh
  session id which Claude Code records immediately, while a monitor keeps the id it was
  armed with for its whole lifetime because monitors cannot be rebound. Measured on a
  machine with fifteen live sessions, two disagreed that way; keying on the session id would
  have refused to deliver to both, for a reason neither registry explains. The process start
  token is identical in both registries and is a strictly stronger guard against a recycled
  pid than the id ever was.

- A message can carry a `subject` and a `brief` alongside its body, so a reader has three
  tiers instead of two. A notification clips the body at about 300 characters, which in the
  run this came from cut every single message on the topic -- the median was 2019 characters
  and roughly three quarters were never read past the prefix -- and sessions had started
  writing shouted opening sentences hoping the cut fell after them. A subject renders whole
  or not at all, ahead of the body, and is never what the give-up ladder drops, so the one
  line a recipient is guaranteed to see is the one the sender chose. An oversize subject is
  rejected rather than truncated, because a silently halved subject leaves the sender
  believing a shorter line arrived intact.

  A brief is the tier in between: a couple of sentences saying whether the rest is worth
  reading. It is what `inbox` shows by default, with `full` there when the detail matters and
  `subject` when triaging. A message with no brief still shows its body under the default
  tier, marked so the reader knows it is seeing everything rather than a summary, since
  falling back to silence would make an unsummarised message invisible. Neither field touches
  the notification budget: a message with a brief renders byte-identically to one without,
  which is asserted rather than assumed. pigeon cannot write these itself, being a Go binary
  with no model behind it, so the tool descriptions ask the sender to.
- An `inbox` MCP tool and `pigeon inbox`, so a session can read its own mail instead of only
  being told about it. Receiving was push-only: one notification line per message, with
  anything longer spilling to a payload file the recipient had to Read by hand, and in the
  run this came from 72% of messages were never read past that prefix -- the pointer was
  offered a thousand times and followed under three hundred. A pull returns the full text for
  a whole burst in one call. Both surfaces render through one function so they cannot drift.

  It needs a second cursor family, because what a monitor has *notified* and what a session
  has *consumed* are different facts. Neither family touches the other: a pull that moved the
  monitor's cursors would silently suppress a notification, and a notification that moved the
  consumption cursors would mark mail read that nobody had seen. Both are seeded together at
  subscribe and at registration -- a monitor advances its ingest cursor within about 200ms of
  a message landing, so an absent consumption cursor falling back on the monitor's position
  finds everything already behind it, and every pull answered "no unread messages" while mail
  sat in the log.

  Compaction learns about them per subscriber: the consumption cursor where that key is
  present, otherwise the monitor's. Taking the minimum over both would stop compaction
  outright, since a missing key reads as offset zero. A consumption cursor also stops counting
  once it is far enough behind, so one session that pulls once and then idles cannot pin a log
  open forever, and `readat` is deliberately left unseeded -- its absence means "never
  pulled", so a session that only ever takes notifications does not hold its topic logs open
  for the whole staleness window. Pulling once opts a session into that protection.
- `set_delivery`, letting a recipient choose how a topic reaches it: `push` (unchanged),
  `digest`, or `quiet`. One session in the run this came from was interrupted 219 times in a
  window where it made 22 commits, and 107 of those messages produced neither a reply nor an
  edit, because there was only one way for a message to arrive. Digest collapses a minute's
  traffic into one line naming the topic, the count and the senders, and the session reads
  them with `inbox` when it chooses. Quiet notifies only that line.

  An alert, or a message naming you in `for`, still interrupts a digest topic -- and never a
  quiet one. That asymmetry is the point: digest says "batch the routine", quiet says "do not
  interrupt me", and a mute a sufficiently insistent peer can override is not a mute.
- An `alert` priority, and exactly two levels. Every notification arrived at the same weight,
  so sessions marked urgency the only way left to them, which was shouting in capitals; because
  everyone could do it, it cancelled out, and a routine commit report and a stop-work order
  looked identical. A third level would recreate that one level down, where the scarce thing
  stops being scarce. The rate limiter reserves ten of its thirty emissions a minute for
  alerts, dropping routine throughput to twenty: a flood of chatter could otherwise suppress
  an alert outright and the session would never learn it existed, which is precisely inverted
  from what the cap is for.
- A `for` list on a publish, saying who a broadcast is actually for. 124 of one session's 219
  inbound messages never mentioned it anywhere in the body, and it eventually gave up and
  started grepping payload files for its own name to decide what to read. The message still
  lands in the topic log in full and still reaches everyone's inbox, so the record stays
  complete and a catch-up read sees what was really said -- but it interrupts only the
  sessions it names. The rest have it in the topic log and in their inbox: the monitor cursor
  crosses it, the consumption cursor does not, so it is there to read and does not cost a turn
  to ignore. Addressing beats alert, because a message urgent enough to escalate is urgent for
  the sessions it names; a sender who means everybody, now, leaves `for` empty.

  Names are stored as typed and matched at delivery, never resolved to session ids when sent.
  A name is only unique among live sessions, so the same string read back later -- on a
  catch-up, or a browse of history -- can mean a different session or nobody at all. Matching
  covers the host label and the full session id as well as a declared name and the short id:
  most sessions never declare a name, and once `for` decides who gets interrupted, "cannot be
  named" quietly becomes "never notified". It fails open when an entry cannot be read, rather
  than muting a session whose registry file was momentarily unavailable, and it is rejected on
  a direct send, where the recipient is already decided and a second list that disagreed with
  it would be a trap.
- `ask` and `answer`, for a question that has to be answered before the asker carries on. A
  coordinator published "is anyone running git right now? Speak up in the next moment,
  otherwise I will remove it", implemented the wait as `sleep 25` in Bash, saw nothing, and
  removed a lock that was live; the replies arrived after the irreversible act and
  contradicted each other. Two things were wrong and only one was the waiting -- the asker was
  told answers were coming and left free to act, and silence was read as consent, which is the
  reading a broadcast can never justify, since a session that says nothing may be deaf, busy,
  or gone.

  So `ask` blocks. It publishes the question as an alert, then tails the answer log itself and
  returns the tally as its own result, which means the asker cannot act before the window
  closes because it is inside a tool call. The deadline lives in the calling process rather
  than in the monitor, which is the component documented as dying on resume without always
  being rearmed, and an ask whose close depended on it would hang exactly when it mattered.

  The audience is snapshotted when the question is asked, so the tally has a fixed
  denominator, and every non-answer is named with what that session's status was at close:
  "no answer (deaf)" and "no answer (live)" mean different things and neither means yes. There
  is deliberately no wording anywhere in the output that reads as "no objections". A session
  that cannot be interrupted is not asked at all, rather than counted and reported as silent.
  An answer from outside the snapshot is recorded and reported separately rather than counted,
  and a session that answers twice replaces its own answer instead of voting twice.
- A message can name one it supersedes. The alarm that prompted this said uncommitted work in
  four files had been destroyed; seventy-seven seconds later its author retracted it, nothing
  had been, and four sessions had stopped by then. The alarm stayed in every recipient's log
  for good, because the only remedy available was a second broadcast -- which is why the
  retraction cost as much as the alarm did. A recipient who has already seen the original is
  now told this is a correction before reading a word of it. One who has not, because the
  topic is on digest and the window has not closed, never sees the original at all: it is
  dropped from the buffer and the alarm simply never happens, and dropping counts as handling
  it so its offset is not lost. That second behaviour belongs to digest rather than to
  supersede -- in push mode the channel drains in milliseconds and the original has almost
  always gone out already, which is worth knowing before anyone expects a retraction to
  overtake its own alarm.

  Only the original sender may supersede, verified against a bounded map of ids this monitor
  has seen. Without that check any peer could cancel someone else's alarm inside a digest
  window, or stamp a false correction frame on a claim that was never withdrawn. An
  unverifiable claim is ignored and the message delivered as an ordinary one, so the failure
  is toward showing too much rather than too little.
- Threads. `ReplyTo` has existed since the beginning and no caller ever set it. Now a reply
  names its parent and the inbox groups a run of them under one header, because settling two
  field names between two sessions took five separate publishes to four readers, and the two
  who had no stake paid a wake for each. The field a sender sets is derived one hop, since a
  sender holds a draft and not the log, so `pigeon thread` walks the reply edges properly
  rather than trusting it. The notification line is untouched: the budget cannot afford a
  thread tag and it would not help at doorbell time, when the reader has not chosen anything
  yet.
- Catch-up on subscribe. A session joining an in-flight topic saw nothing that came before,
  and a coordinator hand-wrote catch-up summaries three times, spending its own context
  reconstructing a log that was sitting on disk. Subscribing can now plant a starting point
  behind the end. Which cursor gets planted is the whole feature: the monitor's stays at the
  end, so joining a busy topic does not fire twenty notifications, and only the consumption
  cursor moves back, so the messages sit in the inbox and the session reads them when it
  chooses. `readat` stays unset, so a session that catches up and then never pulls still does
  not hold compaction open.
- Attachments. A session solved a problem all three of them had and could only offer to
  describe it, there being no way to send the file. Bounded to five files and a quarter
  megabyte each, stored under the message id so two senders attaching the same basename
  cannot overwrite each other. An attachment is a file a peer chose: the tool description says
  to read it rather than run it, and the inbox will only show a path inside a payload
  directory this session already knows, which is the same rule the notification line has
  always applied to a body pointer.
- Every session now starts in the room for the checkout it is working in, alongside the two
  that mean "everyone". A session came up in two rooms both meaning everyone, and the narrower
  room it wanted had to be joined by hand; on the run that prompted this, a project topic
  existed with exactly the right members, two of the three sessions in that checkout had
  joined it, and the third had not -- so it broadcast to the whole machine instead and woke
  nine sessions across six repositories. It was not being careless. Joining is a deliberate
  act, and this codebase keeps finding that sessions do not perform those: nobody set a
  delivery preference either, and nobody used the tool built for asking a question. Defaults
  are the only instruction a session reliably follows, so the narrow room has to be one it is
  already in.

  The room is derived from the git repository root, so a session started in a subdirectory
  lands with its peers rather than alone, and a checkout reached through a symlink resolves to
  the same room. It is an ordinary namespaced topic whose name happens to be computed rather
  than typed: no new tree, no new prefix, nothing that cuts across namespaces, and safe in a
  private namespace for exactly that reason. Two worktrees of one repository get separate
  rooms, which is right when they are separate lines of work and wrong when they are not; the
  basename gets the common case and does not attempt the rest. A project can opt out, because
  the room's name is the directory basename and a private checkout would otherwise put it in
  every peer's copy of its entry and in `list_topics` the moment it used the room.
- A `pigeon-usage` skill, bundled automatically by `pigeon install` alongside the
  monitor and MCP server. Unlike the example skill below, it is strictly
  informational -- the MCP tool list, what `live`/`deaf`/`dead` mean, and a known
  limitation where a monitor that dies is never respawned, so a session that restarts
  comes back under a new address and one that only idles comes back deaf -- so it
  carries no opinion about when to actually
  message another session, which is what makes it safe to bundle without an explicit
  opt-in.
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
  column in `pigeon ls`, in `list_sessions` and `whoami`, and as a `{{.Label}}`
  (and `{{.LabelSource}}`) template field for project configs. It is a host label, not
  an address: routing still keys on the pigeon `name`, and the label is not a send
  target. A derived name mostly echoes the cwd; it earns its place when a session is renamed
  in Claude Code, which pigeon cannot derive on its own.

  It is read best-effort from Claude Code's per-session index
  (`~/.claude/sessions/<pid>.json`, relocated by `CLAUDE_CONFIG_DIR`), keyed by the claude
  pid and verified against the session id before it is trusted, so a recycled pid cannot
  mislabel a session. Like every other undocumented host behaviour pigeon leans on, `pigeon
  doctor` checks it and it degrades to empty rather than failing. A private session withholds
  it along with its cwd, because a derived name would leak the directory.

  The registry entry stores this as `label`/`labelSource`, alongside a new `runtime`/
  `runtimeVersion` pair replacing the earlier `ccVersion` (`runtime` is `claude-code` for
  every entry pigeon writes today). An entry already on disk under the previous
  `claudeName`/`claudeNameSource`/`ccVersion` keys is still read correctly, and self-heals
  onto the new keys the next time its heartbeat rewrites it; `{{.ClaudeName}}` and
  `{{.ClaudeNameSource}}` keep working in a project config as deprecated aliases for
  `{{.Label}}` and `{{.LabelSource}}`.
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

### Changed
- The two rooms that mean "everyone" are renamed. `all` read as "everyone" and meant
  "everyone in this namespace", and since almost nobody runs a second namespace the two were
  the same set in practice -- so the name taught the wrong lesson about which room was which,
  and `@all` looked like a spelling variant of it rather than the different log it is. They
  are now `namespace` and `@global`, and nothing is called "all", so nothing claims to be
  everyone while being something narrower.

  The checkout room gets `here`, the word it was missing. It is named after the repository,
  which is what makes it legible to everybody else, but that left a session unable to name its
  own room without looking it up first and left every document unable to name it at all -- so
  the tier carrying most of the traffic was the only one with no word for it. `here` resolves
  to the real name before anything is written, at the CLI and MCP edges rather than inside
  topic parsing: a subscription list, a cursor key and a prune pass walking log files have no
  session and no cwd behind them, and only a live caller has a "here" to mean. Outside a
  checkout it is left alone rather than silently widening to a room nobody asked for.

  Sessions already running keep the old names until they restart, and the old `all.ndjson`
  stays where it is. Nothing migrates it: the log is readable, the topic is still a valid
  name, and it simply stops being one anybody joins by default.
- A topic name is accepted with the `#` it is printed with. `#` is decoration, not part of the
  name, so every notification said `#chat` and typing `#chat` back failed validation. Output
  that is not valid input only ever bites whoever copies what they were shown, which is what
  an agent does. `@` is unchanged, since it selects a different tree, and `#@ops` stays invalid
  so there is one spelling per tree.
- `publish` reports its audience as a live/deaf split rather than one count. The subscriber
  count walked both session listings and filtered only dead ones, so "3 other live session(s)
  subscribe to it" could mean three sessions none of which would ever see the message -- the
  word "live" was already in the sentence and already untrue. A deaf session is the case a
  sender would otherwise act on, since it only ever reads its spool if it resumes under the
  same session id, and a brand-new session gets a new id. The zero-subscriber note also stops
  reading as reassuring when there are deaf subscribers behind it. `publish` additionally
  warns when a named handle in `for` answers to no live session.
- The host assumptions pigeon leans on are gathered behind a `Runtime` interface with one
  implementation. Sending was never host-specific -- a spool append knows nothing about Claude
  Code and the MCP server speaks an open protocol, so any MCP-capable agent can already
  publish -- but receiving is welded to a background monitor whose stdout a host turns into
  notifications, and those assumptions were spread across the monitor, the spool, the state
  directory, the installer and the session index. An interface designed against a single
  implementation is not an abstraction, so what this buys today is that the surface is visible
  and stops spreading, and that the notification budget is a value the host supplies rather
  than a constant compiled into the rate limiter. The seam is deliberately partial:
  receiving-capability and identity checks go through it, the per-turn "who am I" lookups and
  `Render`'s budget constants do not. No behaviour changes.
- The README and the bundled coordination skill are rewritten around what a message can now be
  and what it costs a reader. The skill matters more than the reference, because it is what a
  session actually reads before deciding how to behave: it leads with the thing that outranks
  every feature in it -- two sessions is the sweet spot, three or more wants a worktree each,
  and do not appoint a coordinator, because at the scale where one seems necessary there are
  already too many sessions and its grounding will be weaker than the specialists it is
  directing. The rest is habits, each written down because its absence cost real hours: say
  what class your evidence is, look for the benign explanation before broadcasting the
  catastrophic one, never run a whole-tree git operation in a shared worktree, and do not act
  on a relayed decision as though the user had said it.

  `ask` is now routed from the situation rather than described as a capability. Across the
  whole life of every session that has coordinated on this machine, `ask` and `answer` were
  called zero times, `set_delivery` zero, and `inbox` once, while the session that had the
  full vocabulary available wrote a direct, answerable question into the body of a publish and
  went back to work -- getting lucky that one of the two peers it named replied unprompted,
  with nothing anywhere recording that an answer was outstanding. The test is not how
  important the message is, it is whether you need an answer before carrying on, and the
  phrases that should trigger it are spelled out.
- `skills/session-coordination` renamed to `skills/pigeon-session-coordination`, so its
  name pairs with the new bundled `pigeon-usage` skill above rather than reading as a
  generic term. Still an example, still not installed automatically; update the path if
  you already copied it under the old name.

### Removed
- `pigeon statusline`. weaverbird renders pigeon's status now, and two commands answering the
  same question is how they drift apart. If you had it wired into `statusLine` in
  `settings.json`, point that at weaverbird and register pigeon as a provider; the README says
  how. Nothing else changes: the widget reports the same states from the same registry.

### Fixed
- A directory was treated as a git checkout on the strength of a `.git` entry existing,
  without checking it was a repository. An empty `.git` directory is not a checkout to git,
  which requires `HEAD`, and it is easy to create by accident -- this was found because one
  existed at `/tmp` on the author's machine. Every session whose working directory sat
  anywhere under it therefore resolved `here` to a room named after that directory, so
  unrelated sessions were placed in a shared room and saw each other's broadcasts. A `.git`
  directory now counts only when it contains `HEAD`; a `.git` file still counts on sight,
  because that is how a linked worktree marks itself.
- A monitor rearm erased every delivery mode the session had set. `register` carried a
  session's name, description and subscriptions across a restart and left `Delivery` out of
  that list, and since `WriteEntry` replaces the whole entry, a field left out is not stale
  but gone. A rearm is not an edge case here: Claude Code kills the monitor on every resume.
  So a session that set a topic to digest or quiet went back to being interrupted by it after
  its next resume, silently, which is the one outcome `set_delivery` exists to prevent. It
  also moved the denominator under `ask`, which excludes quiet sessions from a question's
  audience.
- A payload path reached the notification line and the inbox with its basename unchecked.
  Both surfaces gated on the containing directory, which is pigeon's, and the basename is a
  peer's: it arrives either off a spool line, which this package assumes throughout may have
  been hand-written, or from the name of a file a sender chose to attach. A `]` in it closes
  the bracket hint it sits inside, and a newline -- legal in a POSIX filename, so this needed
  no hand-written line at all -- ends the notification line and starts a second one written
  entirely by the peer. The basename is now bounded to characters that cannot carry
  structure, checked rather than rewritten, since a sanitised path is no longer a path and a
  pointer that does not open is worse than no pointer. An attachment's stored name is bounded
  when it is written as well as when it is shown, because it becomes a real filename on disk.
- `pigeon listen --json` read any file a message named. It inlines a payload's bytes into its
  NDJSON so a consumer never has to open the file, with no check that the path was one pigeon
  wrote -- while `Render` and the inbox both refuse to so much as print a path from outside a
  known payload directory. A hand-written line naming any file the user can read had its
  contents pumped into whatever automation was downstream. It now takes the same containment
  decision the other two take, applied to the read rather than the print.
- An ask tally could be forged. The verdict rows echoed the answering session's name and note
  raw, and an answer log is a file any local process can append to, so the value read back is
  not necessarily the one `answer` sanitised on the way in: a newline in either opened a
  second row, indented to look exactly like the rest -- an agreement from a session that never
  answered, in the one output a coordinator reads before doing something irreversible. Both
  halves are re-sanitised at render time now, the discipline `Render` already applies to a
  name it has validated once. An unrecognised verdict is also no longer accepted at all: it
  was filed with the agreements by the render while the summary counted only the three real
  verdicts, so a forged line could add a row that read as consent and appeared in no tally.
- `here` named a private checkout's own directory. `defaultSubscriptions` refuses to join
  that room for a private project, in a comment naming the hazard exactly -- the room's name
  is the directory basename, which is what `private` keeps off the bus -- but the alias
  resolved without the same check, and the publish tool description steers a session toward
  `here` first. One `publish here` created the topic, hanging the hidden directory's name in
  `list_topics` for the whole namespace, and subscribing wrote it into an entry every peer
  reads, where `WriteEntry` does not blank it. The alias now refuses out loud rather than
  quietly widening to the everyone room, which would send a message meant for one checkout to
  the whole machine. A private namespace is unaffected, its topics being namespace-local.
- The janitor could throw away a live session's mail. The sweep that clears orphaned spools
  and cursors was guarded on age, reasoning that a day is far longer than any monitor gap --
  but a cursor file's mtime records the last time messages flowed, not the last time the
  session was alive, and nothing touches it on a heartbeat. On a quiet machine a running
  session's cursors are days old, so the effective grace was zero: any other session
  registering would sweep them, and cursors are re-seeded at the end of each log on the next
  registration, so everything published during the gap was skipped rather than redelivered.
  Worse, the sweep ran before the registering session wrote its own entry, with no exclusion
  for it, so a monitor rearming after a gap qualified as an orphan and deleted the very spool
  it was about to start following, three lines before recreating it empty.

  The signal is exact now instead of heuristic. Deregistration leaves a tombstone naming the
  process that owned the session, and the sweep asks whether that process is alive: a monitor
  that exited an hour ago while claude runs on keeps its mail, and one whose process is gone
  is collected immediately rather than after a day. Age survives only for orphans that predate
  tombstones, which come from a build that never wrote one and are therefore genuinely old.
  The registering session is excluded by name on top of that.
- A clean exit leaked its spool and cursors. The sweep searches by leftover registry entry, so
  a session killed before it could deregister was cleaned up, while one that exited properly
  removed its own entry first and made everything it owned unreachable: 2796 cursors and 2791
  spools against 9 live sessions, going back three weeks. The tidy path leaked and the messy
  one did not. The sweep already existed and was wired only to `pigeon prune`, which nobody
  types; it runs at registration too now. Deregistration still leaves the spool deliberately,
  so mail queued while nothing was listening survives until a monitor comes back for it.
- A `for` list that matched nobody interrupted nobody, silently. Matching took a declared name,
  a host label and the short id, but not the full session id that `list_sessions` also prints,
  so copying the long form and publishing "shout if you are mid-edit" woke no one while the
  sender was told only how many sessions subscribe to the topic. Silence then reads as consent,
  which is the failure the addressing work exists to prevent.
- The checkout room could leak a private project's directory name. `WriteEntry` blanks a
  private session's cwd, label and description precisely because a derived label is the cwd
  basename -- but the room's name *is* that basename and subscriptions are not blanked, so a
  private checkout would have put it in every peer's copy of its entry and in `list_topics`
  the moment it used the room. A private namespace is unaffected, its topics being
  namespace-local; the per-project flag now opts out.
- A pull could wedge the inbox. Truncating an oversize batch kept the *newest* messages, and
  since dropped and kept messages come from one source, every kept message sat behind a gap
  the cursor could not pass: the cursor never moved, and eight unread with a limit of five
  returned the same five forever while the other three became permanently unreachable.
  Draining takes the oldest now, and a truncated pull says how many are left, because a
  session that reads ten of sixty and stops has lost the other fifty just as surely. A browse
  no longer consumes, since marking read on "show me recent history" made the flag mean
  different things depending on how much history happened to exist. Two overlapping pulls can
  no longer rewind each other's read position, and a pull naming an unsubscribed topic errors
  rather than reading back "no unread messages", which is indistinguishable from the topic
  being quiet.
- The pull path printed sender names, subjects and bodies raw. `Render` sanitises every one of
  those on the notification line precisely because a spool line can be hand-written and never
  pass validation, and the pull path has the same exposure for longer: a newline in a name or
  a body forged extra entries and fake headers inside output the reader treats as pigeon's own
  structure.
- Cursor abandonment is measured in time alone. Cutting once a cursor fell a megabyte behind
  destroyed the case it exists for: pull at 11:58, a peer publishes a megabyte and a half at
  12:00, prune runs at 12:01, and the burst is gone before a session that was reading on time
  ever asked for it. Abandoned has to mean nobody is coming back -- a session that keeps
  pulling closes the gap itself, and one that stops ages out. Peeking does not count as
  reading.
- The deferred digest flush marked messages nobody received. It wrote its line and advanced the
  cursor, and the dominant shutdown here is Claude Code killing the monitor when a session
  resumes -- so the reader on the other end of stdout was already gone, the line landed
  nowhere, and the cursor moved past messages whose only announcement was that lost line. A
  rearmed monitor then resumed past them and nothing ever mentioned them again: precisely the
  silent loss that moving the cursor to handled-time was supposed to end, reintroduced by the
  one path that runs when a session goes away. On the way out the line is now best-effort and
  the cursor stays put. A duplicate digest line after a restart is the cheap side of that
  trade.
- Delivery routed on the message's own topic field, which a peer controls and a hand-written
  line can omit or misname. A line in one topic's log omitting the field took the direct branch
  and advanced that topic's cursor past its own unflushed buffer; a line claiming another
  topic's name flushed that topic's cursor to an offset measured in a different log, and since
  cursors only move forward it would skip its own unread messages permanently. Everything
  routes on the source the follower actually read now, and the message's field is display only.
  The pending-digest check in `advanceCursor` was keyed the same wrong way and was missed when
  the rest moved.
- Digest lines and suppression notices bypassed the emission cap they exist to respect. Claude
  Code kills a monitor that emits too many events, so fifteen digest topics could add
  forty-five uncounted lines a minute on top of thirty counted ones and cost the session its
  monitor -- worse than any suppression. A digest line now spends from the alert reserve, since
  one stands in for many messages and is the opposite of chatter. A suppressed alert's notice
  could also wait indefinitely, being written only from inside the next emit when the shape
  that suppresses an alert is a flood followed by silence; the digest tick now rolls the
  window.
- The cursor recorded ingestion rather than notification. `followSource` persisted right after
  pushing a read pass into the channel, which was invisible while the gap was milliseconds and
  fatal once a digest holds messages for a minute inside a process known to die on resume
  without being rearmed -- a monitor killed mid-window would have resumed past its own
  unflushed buffer and nothing would ever have mentioned those messages again. The channel
  carries the offset with the message now, `followSource` writes no cursors at all, and the
  delivery loop persists one only once the message at it has been handled: pushed, folded into
  a digest that has since flushed, dropped as a self-broadcast, or suppressed by the rate
  limiter. Suppression counts, since the design deliberately does not re-notify a suppressed
  message and holding the cursor there would redeliver it forever. No cursor moves for a topic
  while it has a pending digest, or an alert pushing past an older message still in that
  topic's buffer would jump over it.
- A pull racing compaction redelivered the whole log. Stat, base read and open are three
  syscalls with nothing holding them together, and compaction renames the log before it writes
  the new base, so a pull landing in that window measured one era's base against the other
  era's file: the seek landed short by the size of the cut, every returned offset was
  understated by the same amount, and `MarkRead` persisted it. The next pull then read its own
  cursor as "compacted past us", reset to the base, and redelivered the entire surviving log.
  One retry is enough, since compaction holds the topic lock for the rename and the base write
  together.
- A session's own broadcast left its consumption cursor behind. The monitor refuses to wake a
  session with its own message and advances the monitor cursor to record that it handled it,
  but nothing advanced the consumption cursor -- which is also the floor compaction will not
  cut past, so a session that pulls regularly and then publishes held its topic log open on
  account of its own broadcast, with the cursor's meaning false about the one message the
  session can be surest is dealt with. Only a cursor sitting exactly where the message begins
  may move: jumping it to the end unconditionally would carry it over anything unread in
  front, so a single publish to a busy topic would silently stop every unpulled peer message
  being unread.
- `set_delivery` and `subscribe` wrote against the environment's session id, so after a
  `/clear` they either failed as unregistered or wrote a setting no running monitor reads.
  Both resolve through the same identity resolver as every other write path now, as does the
  inbox itself -- reading the environment's id there would find no cursors, treat the whole
  history as unread, and then write a read position under an id nothing else uses.
- `subscribe` no longer re-seeds the monitor cursor of a topic already followed, which skipped
  anything queued since. Catch-up also no longer confirms a window it did not plant: seeding
  only ever seeds, so subscribing twice -- or unsubscribing and coming back, since that leaves
  cursors behind -- reported twenty messages waiting into an inbox that would show none.
- `ask` and `answer` resolved their namespace from the working directory rather than from the
  entry the monitor is serving, so a divergence made quorum vacuously true and returned
  "nobody was there to ask" while the topic had live subscribers.
- Attachments leaked. Bodies spill as `<id>.txt` and the reclaim pass globbed only that, so an
  attachment keeping its own extension was invisible to it forever -- up to five files and a
  quarter megabyte per message, including orphans from a failed copy. The referenced-set check
  was always the thing deciding safety and the glob only decides what gets considered, so it
  considers everything now. Prune also walked only a message's body overflow when deciding
  what was still referenced, so an attachment could be collected while messages still pointed
  at it.
- A thread came back in a different order run to run. Messages were ordered by timestamp, which
  is one-second resolution, and a thread is precisely where several land inside one second; the
  ties were then broken by Go's randomised map iteration. A conversation is ordered by its
  reply chain now, parent before child, which is both deterministic and what the participants
  meant.
- A flag written after the topic was silently dropped. Go's flag package stops parsing at the
  first positional argument, so `publish topic --subject x body` filed the subject away as
  message text and reported success -- a silent drop, in a tool whose whole purpose is that
  messages arrive.
- A session that has been cleared now identifies itself correctly to the CLI. Same root cause as
  the `not armed` alarm below: after a clear, the monitor and the MCP server still hold the id
  they started with, while `pigeon` run from a shell in that session is handed the new one, and
  under that id nothing is registered. So `whoami` said "not registered", `doctor` failed the
  session as unreachable, `arm` offered to arm a monitor that was already running -- which would
  have put a second one on the same session -- and `send`/`publish` stamped a reply address that
  resolved to no spool, which also defeated the guard that stops a session waking on its own
  broadcast. Every "which session am I" lookup now goes through one resolver that answers from
  the registry rather than the environment, so the CLI, the MCP server and the monitor cannot
  disagree. A session whose monitor genuinely never armed is still reported exactly as before.

  The monitor itself is deliberately not re-registered under the new id. Its identity is fixed
  for its lifetime, like its namespace, and moving a live session's entry, lock, spool and
  cursors mid-flight would race a sender already writing to the old spool -- losing mail to fix
  a labelling problem.
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

[Unreleased]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/PeterSR/claude-code-pigeon/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/PeterSR/claude-code-pigeon/releases/tag/v0.1.0
