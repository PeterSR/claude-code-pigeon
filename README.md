# pigeon

Message passing between live Claude Code sessions.

Sessions can see each other, send directly, and publish to topics. A message reaches its
recipient **even when that session is sitting idle at the prompt** — no polling, no
keystroke, nothing to arm by hand.

```console
$ pigeon ls
   SESSION   NAME   STATUS  CWD                DESCRIPTION
 * aaaa1111  alpha  live    ~/dev/api-server   refactoring the parser
   bbbb2222  beta   live    ~/dev/frontend     -

$ pigeon send alpha "the build is green"
sent -> aaaa1111 (alpha)

$ pigeon publish all "deploying to staging in 5"
published to #all (2 subscriber(s) besides you)
```

The receiving session wakes about a second later:

```
[pigeon] message from beta (frontend) :: the build is green [reply: pigeon send beta]
```

## Install

One static binary is the whole product — CLI, background monitor, and MCP server.

```console
$ go install github.com/PeterSR/claude-code-pigeon/cmd/pigeon@latest
$ pigeon install       # writes the plugin
# restart Claude Code
```

`pigeon install` scaffolds a plugin at `~/.claude/skills/pigeon`, which auto-loads as
`pigeon@skills-dir`. There is no marketplace to add and nothing to clone. Every session
from then on registers itself at startup.

Monitors cannot be rebound mid-session, so the restart is required.

### Arming one session without the plugin

Manual arming is a first-class path, not a fallback — the monitor takes nothing from the
plugin at runtime:

```console
$ pigeon arm
```

It prints the exact `Monitor(command=…, persistent=true)` call to run. Same monitor, same
capabilities; the plugin just saves you doing it per session.

## Use

```
pigeon ls [--all] [--json]      list sessions and whether they are listening
pigeon send <target> <text>     send to one session
pigeon publish <topic> <text>   publish to everyone subscribed
pigeon subscribe <topic>        join a topic (takes effect in ~1s, no restart)
pigeon unsubscribe <topic>
pigeon topics                   topics and subscriber counts
pigeon name <name>              declare a name, usable as an address
pigeon describe <text>          declare what this session is working on
pigeon whoami                   this session's identity and address
pigeon prune                    forget sessions whose process is gone
```

Targets resolve as **exact session id → declared name → id prefix → cwd basename**.

Sender identity is attached automatically. A session never has to know its own address for
replies to work; a message from a plain shell is stamped `shell:user@host` and carries no
reply handle, because there is nowhere to reply to.

### Topics

Every session joins `all` by default, so `pigeon publish all "…"` broadcasts to the
machine. Topics are append-only logs and each subscriber keeps its own cursor, so
publishing is O(1) regardless of how many sessions listen, and nobody consumes anyone
else's messages. Subscribing starts from now — history is not replayed into your context.

### From MCP

The plugin exposes `list_sessions`, `send_message`, `publish`, `subscribe`,
`unsubscribe`, `list_topics`, `whoami` and `set_identity`, so a session can do all of this
itself.

## Knowing when a session has stopped listening

A session can be running while its monitor is not. `pigeon ls` reports three states:

| Status | Meaning |
|---|---|
| `live` | monitor is listening; a message arrives in about a second |
| `deaf` | the session is running but nothing is listening — messages queue undelivered |
| `dead` | the process is gone; `pigeon prune` clears it |

This is not a heuristic. The monitor holds an exclusive `flock` for its entire lifetime and
the kernel releases it the instant that process exits — cleanly, crashed, or `SIGKILL`ed —
so any other process can detect a dead monitor just by trying to take the lock. A stale
heartbeat catches the rarer case of a monitor that is alive but wedged.

Sending to a `deaf` session warns you, and the message stays on the spool.

## Opting a session out

Set `PIGEON=0` in its environment. Intended for programmatically driven sessions
(claude-p, pupptyeer, CI) where a driver already owns the conversation. The launcher knows
how it started the session, so it declares it — pigeon does not try to infer it.

## How it works

Claude Code plugins can declare background monitors that the host starts at session start,
with no model involvement. Every line such a monitor prints to stdout is delivered to that
session as a `<task_notification>`, which wakes it if idle.

pigeon's monitor follows two kinds of source: the session's own inbox spool, and one log
per subscribed topic. It identifies itself from `CLAUDE_CODE_SESSION_ID`, which Claude Code
injects into the processes it spawns. If that variable is missing it **fails loudly** rather
than guessing from the process tree — a wrong guess would deliver another session's mail.

State lives in `~/.claude/pigeon` (`PIGEON_HOME` to relocate), owner-only:

```
sessions/<id>.json     registry entry
inbox/<id>.ndjson      direct messages
topics/<topic>.ndjson  shared topic log
cursors/<id>.json      per-session read offsets
locks/<id>.lock        liveness
payloads/<msg>.txt     bodies too long to inline
```

Notifications are clipped at about 512 characters by Claude Code, so messages over ~300
are written to `payloads/` and the recipient gets a path instead.

## Limits

- Interactive sessions only. Headless `claude -p` arms no monitors.
- Unix only.
- One machine. No network transport.
- 30 notifications per minute per session; beyond that pigeon reports suppression rather
  than being stopped by Claude Code.
- Unauthenticated: anyone who can write your `~/.claude/pigeon` can inject text into your
  sessions. See [SECURITY.md](SECURITY.md).

## Prior art

pigeon builds on work by others, and depending on your needs one of them may suit you
better — encryption, non-Claude agents, or a broader context server.
[docs/ALTERNATIVES.md](docs/ALTERNATIVES.md) compares them honestly and collects the
mechanism's many sharp edges.

## Licence

MIT.
