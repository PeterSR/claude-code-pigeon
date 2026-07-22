# Prior art and alternatives

pigeon is not the first project to move messages between Claude Code sessions, and
depending on what you need, one of these may suit you better. This page exists so you can
make that call yourself.

Everything here was surveyed in July 2026 against Claude Code 2.1.217.

## What pigeon owes to others

**[xhluca/agent-talk](https://github.com/xhluca/agent-talk)** (MIT) — the architecture.
agent-talk got there first: a plugin-declared monitor with `when: "always"`, blocking on
`tail -F` of a per-session NDJSON spool, one spool line per notification. pigeon's monitor
is the same shape, reimplemented in Go. If you want encrypted agent-to-agent messaging
built on `retalk`, and cross-agent support for pi/opencode, use agent-talk instead.

**[yilunzhang/claude-code-inter-session](https://github.com/yilunzhang/claude-code-inter-session)**
— two ideas. First, holding a lock for the monitor's lifetime and *never unlinking it*,
because unlinking lets a second process lock a different inode while both believe they
hold it. Second, the 400-character budget: it measured Claude Code's notification clip
byte-by-byte ([issue #2](https://github.com/yilunzhang/claude-code-inter-session/issues/2))
so nobody else has to.

**[Goldziher/basemind](https://github.com/Goldziher/basemind)** — the belt-and-braces
pattern of pairing a monitor (idle push) with a hook (turn-boundary catch-up). pigeon does
not ship the hook half yet.

**[modem-dev/sideshow](https://github.com/modem-dev/sideshow)** — its
`docs/plans/claude-code-plugin.md` is the best third-party write-up of the plugin-monitor
mechanism in existence. Read it if you want to understand the substrate.

## How they compare

|  | pigeon | agent-talk | inter-session | basemind |
|---|---|---|---|---|
| Wakes an idle session | yes | yes | yes | yes |
| Polling in the session | none | none | none | 15 s loop |
| Arms without the model acting | yes | yes | in practice no | yes |
| Addressed by | session id, name, cwd | user | user-chosen name | cwd |
| Two sessions in one directory | distinct | share an inbox | distinct | share an inbox |
| Send from a plain shell | yes | yes | **no** | yes |
| Pub/sub topics | yes | no | broadcast only | rooms |
| Detects a dead monitor | yes | no | no | no |
| MCP server | yes | no | no | yes |
| Encryption | no | yes | no | no |
| Cross-agent (opencode, pi) | no | yes | no | yes |
| Language | Go, single binary | bash + retalk | Python | Rust |

**Pick agent-talk** for encryption or non-Claude agents. **Pick basemind** if you want a
broader context server and don't mind a poll loop. **Pick inter-session** to read a careful
process-tree implementation. **Pick pigeon** if you want one static binary, session-id
addressing, topics, and a way to tell when a session has stopped listening.

## Things everyone gets wrong

Collected so you don't rediscover them.

**The session id variable is `CLAUDE_CODE_SESSION_ID`.** There is also a
`CLAUDE_SESSION_ID` in the binary, but it is vestigial and never set. agent-talk reads the
latter, so its monitor's guard clause matches the unexpanded literal and it
`exec tail -f /dev/null`s forever — the push channel silently does nothing and only its
pull-based path works. One word.

**Do not interpolate `${CLAUDE_CODE_SESSION_ID}` in `monitors.json`.** Manifest
substitution reads Claude Code's *own* environment, which carries no `CLAUDE_*` variables,
so it expands to nothing. The spawned monitor *does* inherit them. Read it inside the
script.

**~512 characters per notification, hard.** Measured: 462 bytes of payload plus a 49-byte
prefix arrives whole; one more byte appends `...(truncated)`. Send a pointer, not a
payload.

**Project-scope plugins load no monitors at all.** Personal scope only. pigeon installs to
`~/.claude/skills/pigeon` for this reason.

**`${user_config.*}` is rejected in monitor commands since 2.1.207** (shell-injection fix),
and monitors never receive `CLAUDE_PLUGIN_OPTION_*`. This silently broke sideshow and every
monitor in `Magic-Man-us/claude-code-monitor-examples`. Read config from a file you own.

**`/reload-plugins` does not rebind monitors** — a plugin update mid-session leaves the
monitor on the old path, and only a session restart fixes it.

**Headless `-p` arms no monitors.** Documented: they run only in interactive CLI sessions.

**Monitors that emit too much are stopped automatically.** pigeon rate-limits to 30
notifications a minute and reports suppression rather than getting killed.

**Waking a session with an imperative makes it hallucinate a user turn.**
[anthropics/claude-code#60360](https://github.com/anthropics/claude-code/issues/60360):
the model echoes `Human:` blocks or fabricates an entire fake user message that persists
in the transcript. pigeon phrases every notification as a report about an event.

**The notification line is a prompt-injection surface, and it is unsolved in the wild.**
inter-session has two open issues on exactly this: unescaped structural characters allowing
a forged trailing directive (#7) and unescaped labels enabling sender spoofing (#6).
Meanwhile `sankalpgunturi/collab-claw` deliberately instructs the model to treat incoming
lines as *"a new user request from that named teammate"*. pigeon neutralises `<`, `>`,
newlines and control characters, and never tells the model to obey what arrives. See
[SECURITY.md](../SECURITY.md).

**Monitors are unavailable** when `DISABLE_TELEMETRY` or
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is set, and on Bedrock, GCP Agent Platform and
MS Foundry.

## Approaches pigeon rejected

**Binary patching.** Splicing the Claude Code bundle to reach the command queue directly
works and would additionally allow interrupting an in-flight turn (`priority:"now"`), but
it means chasing minified anchors across releases. Not worth it when a documented plugin
mechanism does the job.

**`.claude/scheduled_tasks.json`.** Claude Code runs a chokidar watcher on that file, so an
externally written task wakes a session with no plugin at all. Rejected because targeting
is unreliable: one shared file, one lock per directory, and an orphan-adoption rule that
lets the lock holder claim tasks addressed to another session. It also sits behind a
server-side feature gate.

**Process-tree walking to find the session.** Both inter-session and sideshow do this well,
and pigeon would have needed it if `CLAUDE_CODE_SESSION_ID` were absent. It isn't, and
start-time heuristics misidentify idle sessions confidently — a wrong guess delivers
another session's mail. pigeon fails loudly instead.

**FIFOs.** A FIFO drops writes when no reader is attached and races between two readers.
Every inbound project converged on append-only spools.
