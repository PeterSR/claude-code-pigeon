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
```

Pre-built binaries for Linux, macOS and FreeBSD (amd64 and arm64) are attached to each
tagged [release](https://github.com/PeterSR/claude-code-pigeon/releases); download one,
put it on your `PATH`, and skip the build.

Either way, then:

```console
$ pigeon install       # writes the plugin
# restart Claude Code
$ pigeon doctor        # confirm this session can actually receive
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
pigeon doctor [--json]          check whether this session can receive mail
pigeon statusline [--plain]     one-line alarm for a Claude Code statusline
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

### An example skill

`skills/session-coordination/` teaches a session when to reach for these tools, and
how to treat what arrives. It is **not** installed by `pigeon install` — skills change
model behaviour, so opting in should be deliberate. Copy it to `~/.claude/skills/` if
you want it. See [skills/README.md](skills/README.md).

## Knowing when a session has stopped listening

A session can be running while its monitor is not. `pigeon ls` reports three states:

| Status | Meaning |
|---|---|
| `live` | monitor is listening; a message arrives in about a second |
| `deaf` | the session is running but nothing is listening — see below |
| `dead` | the process is gone; `pigeon prune` clears it |

This is not a heuristic. The monitor holds an exclusive `flock` for its entire lifetime and
the kernel releases it the instant that process exits — cleanly, crashed, or `SIGKILL`ed —
so any other process can detect a dead monitor just by trying to take the lock. A stale
heartbeat catches the rarer case of a monitor that is alive but wedged.

Sending to a `deaf` session warns you. The message is kept on that session's spool, and a
monitor that starts later for **the same session id** (`claude --resume`) will read it. A
brand-new session gets a new id and will not see it, so treat `deaf` as "probably will not
arrive" rather than "will arrive eventually". `pigeon prune` clears the spool once the
process is gone.

### Diagnosing it: `pigeon doctor`

Delivery is a chain — session id, state directory, plugin, monitor binary, registration —
and when a link breaks the symptom is always the same: messages stop arriving and nothing
says why. `doctor` checks each link separately and names the one that broke.

```console
$ pigeon doctor
ok    session         9f3c1a20
ok    state dir       /home/you/.claude/pigeon
ok    plugin          /home/you/.claude/skills/pigeon
warn  monitor binary  plugin runs /usr/local/bin/pigeon, but this is /home/you/go/bin/pigeon
                      -> sessions arm the plugin's copy, not this one; run `pigeon install` to point it here
FAIL  this session    registered but no monitor is listening
                      -> the monitor died or never started; restart the session, or run `pigeon arm`
```

It exits non-zero if anything is a `FAIL`, so it works in a health check. `--json` gives
the same checks machine-readably.

The `monitor binary` check is worth calling out: `go install` writes a new binary while
the plugin keeps pointing at wherever the old one lived, so sessions silently keep arming
the stale copy. Nothing else reports that.

### Seeing it: `pigeon statusline`

```json
{
  "statusLine": { "type": "command", "command": "pigeon statusline" }
}
```

**A healthy session renders nothing at all.** There is deliberately no peer list and no
unread count: a live monitor drains the spool within about a second, so there is never a
standing backlog to display, and listing whichever other sessions happen to be running
fills the line with work you are not doing. A statusline that is always lit becomes
wallpaper, and wallpaper is what you stop reading before the one time it mattered.

It renders only when this session **cannot** receive:

```
🕊 deaf · 3 waiting     monitor stopped; mail is piling up on the spool
🕊 not armed            the monitor never started for this session
```

Those are the only states where a count is real, and the only ones nothing else in the UI
reports. If you already have a statusline, append pigeon's output to it — the command
prints one line or nothing, so concatenating is safe:

```bash
#!/bin/bash
input=$(cat)
line=$(your-existing-statusline <<<"$input")
alarm=$(pigeon statusline <<<"$input")
printf '%s%s' "$line" "${alarm:+ $alarm}"
```

`--plain` drops the emoji and colour.

## Opting a session out

Set `PIGEON=0` in its environment. Intended for programmatically driven sessions
(claude-p, pupptyeer, CI) where a driver already owns the conversation. The launcher knows
how it started the session, so it declares it — pigeon does not try to infer it.

## How it works

Claude Code plugins can declare background monitors that the host starts at session start,
with no model involvement. Every line such a monitor prints to stdout is delivered to that
session as a `<task_notification>`, which wakes it if idle.

**This behaviour is shipped but undocumented.** That monitors are started at all, that
`CLAUDE_CODE_SESSION_ID` is injected into them, and that their stdout becomes a
notification are all observed, not promised — verified end to end against Claude Code
2.1.217, and requiring 2.1.105 or newer for the session id. A future release could change
any of it, and the failure would be silent. That is what `pigeon doctor` is for: it checks
each assumption separately and warns when your Claude Code is newer than the version this
was tested against.

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
- Receiving is Unix only. The binary builds and sends from Windows, but Claude Code does
  not arm plugin monitors there, so a Windows session cannot receive.
- PID-reuse detection needs Linux `/proc`; elsewhere a recycled PID can make a dead
  session look alive until something prunes it.
- `pigeon prune` does not compact topic logs on Windows. Compaction replaces a log by
  rename, which Windows refuses while any process still holds it open. Forgetting dead
  sessions still works; only the space reclaim is skipped.
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
