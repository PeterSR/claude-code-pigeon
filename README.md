# pigeon

Message passing between live Claude Code sessions.

Sessions can see each other, send directly, and publish to topics. A message reaches its
recipient **even when that session is sitting idle at the prompt** — no polling, no
keystroke, nothing to arm by hand.

```console
$ pigeon ls
   SESSION   NAME   CLAUDE       PID    STATUS  CWD               DESCRIPTION
 * aaaa1111  alpha  api-server   41822  live    ~/dev/api-server  refactoring the parser
   bbbb2222  beta   frontend-b3  41977  live    ~/dev/frontend    -

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
from then on registers itself at startup, and carries a bundled `pigeon-usage` skill --
the MCP tool list, status meanings, known limitations -- so a session does not have to
discover any of that on its own. See [Skills](#skills).

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
pigeon listen [topic...]        receive messages in this shell, blocking
pigeon topics                   topics and subscriber counts
pigeon namespaces               namespaces and their session counts
pigeon namespace [<name>]       show or set the namespace this shell uses
pigeon as [<name>]              show or set the identity this shell acts as
pigeon name [<name>]            declare a name, usable as an address
pigeon describe [<text>]        declare what this session is working on
pigeon whoami                   this session's identity and address
pigeon doctor [--json]          check whether this session can receive mail
pigeon weaverbird spec|value    status widgets for a weaverbird status line
pigeon prune                    forget sessions whose process is gone
```

Targets resolve as **exact session id → pid → declared name → id prefix → cwd
basename**, within one [namespace](#namespaces). `ls`, `send`, `publish`, `topics` and `prune` take
`-n`/`--namespace <ns>`; `ls`, `topics` and `prune` also take `--all-namespaces`.

Sender identity is attached automatically. A session never has to know its own address for
replies to work; a message from a plain shell is stamped `shell:user@host` and carries no
reply handle, because there is nowhere to reply to. From a shell that is
[acting as an inbox](#listening-from-a-shell-automation) it is stamped as that inbox, so a
reply reaches it.

`name` and `describe` also take `--template '{{.Dir}}-{{.Seq}}'`, rendered against this
session. The fields and functions are the ones in [Project defaults](#project-defaults)
below.

### Project defaults

A project can declare what sessions started in it should look like, in
`.claude/pigeon.json`:

```json
{
  "name": "api",
  "description": "the payments service",
  "topics": ["deploys", "ci"]
}
```

A session opened in that checkout comes up already named `api`, already described, and
already listening to `#deploys` and `#ci` alongside the default `#all` and `@all`. Commit
the file and everyone working on the repo gets the same wiring without configuring
anything.

A `"namespace": "acme"` field puts sessions started here in their own group, isolated from
every other one. Unlike the fields below it is not a template: the namespace decides which
directory a session's entry, spool and cursors are written to, so it has to be known
before there is a session to render anything against. See [Namespaces](#namespaces).

Two rules make it predictable:

- **The config seeds a session; it does not own it.** It is read only at a session's
  first registration. After that `pigeon name`, `pigeon describe` and
  `pigeon unsubscribe` are authoritative, so your own choices are not undone the next
  time a monitor starts for that session.
- **A taken name is not stolen.** A name is an address, so if another live session
  already holds it the new session stays unnamed rather than making replies ambiguous.
  It is still reachable by session id, and the monitor log says what happened.

#### Naming a session after what it is

The useful defaults are the ones a committed file cannot know: which directory, which
branch, how many sessions are already open here. So `name`, `description` and
`onNameTaken` are Go [`text/template`](https://pkg.go.dev/text/template) source, rendered
per session:

```json
{
  "name": "{{.Dir | kebab}}",
  "onNameTaken": "{{.Name}}-{{.Seq}}",
  "description": "{{.Dir}} on {{.Branch | default \"no branch\"}}",
  "topics": ["deploys", "ci"]
}
```

A string with no `{{` is a literal, so plain `"name": "api"` keeps working and nothing
has to be escaped.

| Field | What it is |
|---|---|
| `.Cwd` | full path of the project directory |
| `.Dir` | its basename |
| `.Branch` | the checked-out branch, read straight from `.git/HEAD`. A detached HEAD gives the short commit; anywhere that is not a repository gives `""` |
| `.Host` | hostname |
| `.User` | login name |
| `.Session` | full session id |
| `.Short` | its first 8 characters, the form `pigeon ls` shows |
| `.Seq` | 1 plus the number of live sessions already in this directory |
| `.Name` | in `onNameTaken` only: the name that was already taken |
| `.Label` | the host's own session name, the one `/status` shows for Claude Code. Filled in only for the current session; empty otherwise. See [below](#the-pid-and-claude-codes-own-session-name) |
| `.LabelSource` | how the host arrived at it: `derived` from the cwd, or a value that marks it user-set |
| `.ClaudeName`, `.ClaudeNameSource` | deprecated aliases for `.Label` and `.LabelSource`, kept working for a project config written before the rename |

| Function | What it does |
|---|---|
| `snake` | lowercases, and turns every run of anything that is not a letter or digit into `_` |
| `kebab` | the same, with `-` |
| `lower`, `upper` | case |
| `trunc N` | keep the first N characters: `{{.Dir \| trunc 8}}` |
| `default "x"` | `x` when the value is empty |

`snake` and `kebab` exist because a real branch is not a real address: `feature/API v2`
has to become `feature-api-v2` before anything can be sent to it.

`onNameTaken` is tried **once**, and only when the rendered name is already held by
another live session. If it is absent, fails to render, or renders to a name that is also
taken, the session stays unnamed and the monitor log says why. There is no loop that hunts
for a free name, because the name it eventually found would be an address nobody declared.
With `{{.Name}}-{{.Seq}}` the second session in a checkout comes up as `api-2`, which is
the case the field exists for.

#### Keeping a project quiet

```json
{ "enabled": false }
```

Sessions started in this checkout stay off the bus entirely, the same as setting
`PIGEON=0` in their environment. The environment still wins in both directions: `PIGEON=1`
keeps a session addressable in a project that disables itself, and `PIGEON=0` still opts
one out of a project that does not.

```json
{ "private": true }
```

The session registers and stays addressable by name and session id, but its working
directory and description are never published. Neither appears in another session's
`pigeon ls`, in `list_sessions`, or in the notification line a message from it produces.
Intended for client work you would rather not have surfaced in an unrelated window. The
withholding is enforced on every write to the entry, not just at startup, so a later
`pigeon describe` does not quietly undo it. One consequence worth knowing: `.Seq` counts
sessions by working directory, so it cannot see private ones.

`pigeon doctor` reports which config was found and what it applied. It reports the
*rendered* values, since with templates the file no longer contains them, so a session
named by a file in the repo is never a mystery.

Because the file arrives with a `git clone`, every field in it is validated exactly as
strictly as one typed at the CLI: a name that is not a valid address is rejected rather
than sanitised, the description is flattened and bounded, topic names are checked, and
the topic count is capped. One bad field is dropped and reported; it does not cost you
the rest of the file.

That applies to the templates too, which are a hostile repository's most obvious lever:
a name and description are rendered into *other* sessions' notification lines. The source
is length-bounded before it is parsed, execution writes into a bounded buffer so no
template can produce unbounded output, and the rendered name is put through the same
validation a typed one is, and rejected rather than repaired. A template that fails to
parse, fails to render, or renders to something unusable is a reported problem and never a
failed registration: the session still comes up, unnamed, and the monitor log says what
happened.

### The pid and Claude Code's own session name

`pigeon ls` shows two more columns, and both also appear in `list_sessions` and
`whoami`.

`PID` is the claude process id. It is a valid send target too, so `pigeon send
41822 "..."` reaches that session. A pid is exact and unique among live
sessions, so it resolves ahead of the fuzzier name, prefix and cwd tiers; it is
the handle to reach when a session is unnamed and you already have its pid from
`ps` or the OS.

`CLAUDE` is the name Claude Code gives the session itself, the one `/status`
shows. It is a label, not an address: routing keys on the pigeon `name` you
declare, never on this, and it is not accepted as a send target. By default
Claude Code derives it from the working directory, so it mostly echoes what
`cwd` already says; it earns its column when you rename a session in Claude
Code, which is the one name pigeon cannot derive for you. Templates can read it
as `{{.Label}}`, so a workflow built around those renames can adopt it with
`"name": "{{.Label}}"` (`{{.ClaudeName}}` still works, as a deprecated alias).

pigeon reads it from Claude Code's own per-session index, which is another piece
of shipped-but-undocumented behaviour (see [How it works](#how-it-works)), so it
fails soft: if a release moves the file the column falls back to empty, `pigeon
doctor` reports whether the read works, and nothing else breaks. A private
session withholds it along with its cwd, because a derived name would leak the
directory the session asked to keep off the bus.

### Topics

Every session joins `all` by default, so `pigeon publish all "…"` broadcasts to your
namespace, and `@all` for the whole machine. Topics are append-only logs and each
subscriber keeps its own cursor, so publishing is O(1) regardless of how many sessions
listen, and nobody consumes anyone else's messages. Subscribing starts from now --
history is not replayed into your context.

A plain name is one log per namespace; a name written `@ops` is one log the whole machine
shares. See [Namespaces](#namespaces).

### Listening from a shell (automation)

`pigeon send` and `pigeon publish` already work from any shell. `pigeon listen` is the
other half: it blocks, printing messages as they arrive, so a script outside Claude Code
can subscribe to topics and react.

```console
$ pigeon listen deploys ci          # tail two topics, print each message
$ pigeon listen deploys | jq .text  # NDJSON when piped, so jq can read it
```

At a terminal it prints the same human line a session would see; piped, it emits one JSON
object per line (`--json` and `--plain` force either). By default it delivers only messages
that arrive while it is listening; `--replay` drains what is already on the log. `--count N`
stops after N messages and `--timeout 30s` after a while, both handy in a script.

Give it an identity and it becomes a **visible inbox**: it registers an ephemeral session,
shows up in `pigeon ls` (as a `shell`), and is addressable as `pigeon send <name>` like any
peer. It vanishes when the shell exits.

```console
$ pigeon as inbox                   # a standing identity for this shell
$ pigeon listen all deploys         # opens the inbox "inbox", follows the topics
```
```console
# meanwhile, from anywhere:
$ pigeon send inbox "restart the worker"
$ pigeon ls
   SESSION   NAME   CLAUDE  PID     STATUS  CWD          DESCRIPTION
   listen-i  inbox  shell   41990   live    ~/ops        -
```

The identity has three spellings, mirroring `pigeon namespace`: a standing `pigeon as
<name>` (kept in `~/.claude/pigeon/cli.json`, so it survives across shells and does not care
where you run from), a `--as <name>` flag on `listen`/`send`/`publish` for one call, and a
`PIGEON_AS` environment variable for one script. Highest wins: `--as`, then a real Claude
Code session, then `PIGEON_AS`, then `pigeon as`, then a plain shell. Once set, that shell's
`send` and `publish` are stamped as the inbox so replies route back to it -- but only while
the inbox is actually listening; with nothing holding it open the post falls back to a plain
`shell:user@host` with no reply address, so a standing identity is never a promise nothing
can keep.

### From MCP

The plugin exposes `list_sessions`, `send_message`, `publish`, `subscribe`,
`unsubscribe`, `list_topics`, `list_namespaces`, `whoami` and `set_identity`, so a session
can do all of this itself. `set_identity` takes `nameTemplate` and `descriptionTemplate`
alongside the literal fields, with the same context and functions as the project config.
`list_sessions`, `send_message`, `publish`, `subscribe` and `unsubscribe` take an optional
`namespace`; leaving it out means this session's own.

## Skills

Two exist, treated differently on purpose.

`pigeon-usage` is bundled by `pigeon install` itself: the MCP tool list, status
meanings, and known platform limitations, such as a monitor that dies not being
respawned mid-session. It carries no opinion about *when* to
actually message another session, which is what makes it safe to install as a side
effect of running a binary.

`skills/pigeon-session-coordination/` does carry opinions -- when to reach for these
tools, how to treat what arrives, conventions like short names and asking before
broadcasting. It is **not** installed by `pigeon install`, precisely because those are
opinions rather than facts and opting into someone else's should be deliberate. Copy it
to `~/.claude/skills/` if you want it. See [skills/README.md](skills/README.md).

## Namespaces

A namespace is an isolated group of sessions. Everyone who never thinks about them is in
`default` together, which is the whole of the previous behaviour.

```console
$ pigeon namespaces
   NAMESPACE  LIVE  DEAF
 * default    2     0
   acme       1     1

$ pigeon ls
   SESSION   NAME   CLAUDE       PID    STATUS  CWD               DESCRIPTION
 * aaaa1111  alpha  api-server   41822  live    ~/dev/api-server  refactoring the parser
   bbbb2222  beta   frontend-b3  41977  live    ~/dev/frontend    -

2 session(s) in 1 other namespace(s) (--all-namespaces)
```

### The isolation is structural, not a filter

Each namespace owns a complete state tree:

```
~/.claude/pigeon/
  namespaces/
    default/  sessions/ inbox/ topics/ cursors/ locks/ payloads/
    acme/     sessions/ inbox/ topics/ cursors/ locks/ payloads/
  shared/
    topics/ payloads/ locks/
```

A session in `acme` cannot see `default`'s registry because that is not the directory it
reads. The alternative -- a field on each entry plus a filter -- would put the rule in
`ListSessions`, target resolution, name uniqueness, topic listing, the publish subscriber
count, prune, doctor and every MCP tool. Miss one and it leaks somebody else's sessions.
Per-namespace name uniqueness then falls out for free: two people can both call a session
`api` without either losing its address.

The last line of `pigeon ls` counts what is deliberately out of sight, and appears only
when there is something to count. Isolation you have forgotten about looks exactly like
an empty machine, which is the one way this feature can waste your afternoon.

### Two kinds of topic

| Written | Log | Reaches |
|---|---|---|
| `deploys` | `namespaces/<ns>/topics/deploys.ndjson` | subscribers in your namespace |
| `@deploys` | `shared/topics/deploys.ndjson` | subscribers in every namespace |

The `@` works everywhere a topic is accepted: `pigeon publish @ops`, `pigeon subscribe
@ops`, `"topics": ["@ops"]` in a project config, and the MCP tools. `*` globs and `!`
history-expands in a shell, and `~` gets tilde-expanded, so those would fail in ways that
have nothing to do with pigeon; `@` is shell-safe and reads differently from the `#` a
namespaced topic renders with.

**Every session subscribes to `@all` as well as `all`, and that is deliberate.** It is the
one place the isolation is not absolute: a broadcast meant for everyone on the machine has
to reach everyone on the machine. If you would rather not hear it, `pigeon unsubscribe
@all`.

A notification names the sender's namespace exactly when the message could have come from
outside yours, because that is when it changes how you should read it and where a reply
has to go:

```
[pigeon #deploys] from alpha (api) :: v2.1 rolled out [reply: pigeon send alpha]
[pigeon @ops] from alpha (api) [ns: acme] :: all hands [reply: pigeon send -n acme alpha]
```

Anywhere else the namespace is a constant, and a constant in every notification is noise.

### Where a namespace comes from

Highest first:

1. `PIGEON_NAMESPACE` in the environment
2. `"namespace": "acme"` in the project's `.claude/pigeon.json`
3. `pigeon namespace <name>`, recorded in `~/.claude/pigeon/cli.json`
4. `default`

A launcher knows how it started a session, which beats a file that arrived with a clone;
a checkout knows what it is, which beats a standing preference you set weeks ago. Names
are validated like topic names, because a namespace becomes a directory.

`pigeon namespace` with no argument prints the current one on stdout and where it came
from on stderr, so `$(pigeon namespace)` stays usable and a surprise is still explained.

### A session's namespace is fixed when its monitor arms

For the same reason monitors cannot be rebound mid-session: from the moment it arms, the
monitor holds a lock in that namespace's directory and follows that namespace's topics.
Changing where a session lives means restarting it.

`pigeon namespace acme` is `kubectl config set-context`, not a live move. It changes where
*shell* invocations look and where the *next* session started here will register. It does
not move anything that is already running, and it says so.

One consequence worth knowing: if a project config changes namespace, a restarted session
leaves its old entry behind in a namespace nothing else looks at. `pigeon prune
--all-namespaces` clears it.

### Sending across on purpose

`pigeon send -n acme beta "…"` works. Blocking it would buy inconvenience rather than
isolation: anyone who can write your state directory could append to that spool by hand
anyway. See [SECURITY.md](SECURITY.md). The message lands in the recipient's tree, and the
reply hint it renders carries `-n` so the answer comes back to the right place.

### Upgrading

The on-disk layout changed in 0.2.0. The first pigeon command after the upgrade moves the six state
directories into `namespaces/default/`, once, under a lock, and says so on stderr. Live
sessions keep their queued mail, their cursors and their addresses.

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

Delivery is a chain of links -- session id, state directory, plugin, monitor binary,
registration --
and when a link breaks the symptom is always the same: messages stop arriving and nothing
says why. `doctor` checks each link separately and names the one that broke.

```console
$ pigeon doctor
ok    session         9f3c1a20
ok    namespace       acme (from /home/you/dev/api-server/.claude/pigeon.json)
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

### Seeing it: the status line

pigeon reports its state to [weaverbird](https://github.com/PeterSR/claude-code-weaverbird),
which owns the status line and composes it from every provider you have installed. Register
pigeon by dropping a provider file in `~/.claude/weaverbird/providers/`:

```json
{
  "v": 1,
  "name": "pigeon",
  "spec":  { "exec": ["pigeon", "weaverbird", "spec"] },
  "value": { "exec": ["pigeon", "weaverbird", "value"] }
}
```

**A healthy session renders nothing at all.** There is deliberately no peer list and no
unread count on the default widget: a live monitor drains the spool within about a second,
so there is never a standing backlog to display, and listing whichever other sessions
happen to be running fills the line with work you are not doing. A status line that is
always lit becomes wallpaper, and wallpaper is what you stop reading before the one time
it mattered.

`pigeon.wait` renders only when this session **cannot** receive:

```
🕊 3 waiting     monitor stopped; mail is piling up on the spool
🕊 not armed     the monitor never started for this session
```

Those are the only states where a count is real, and the only ones nothing else in the UI
reports.

Two further widgets are opt-in, for a layout that asks for them by id. They answer
questions worth asking rather than alarms worth interrupting for, so they stay out of the
default view:

| widget | shows |
| --- | --- |
| `pigeon.monitor` | `monitor live`, `deaf` or `dead` for this session |
| `pigeon.peers` | how many other live sessions share this namespace |

`pigeon.detail` is a group covering all three, so a layout can ask for one id instead of
three.

### Private namespaces

A namespace can be marked private in your machine config, which lives where user
configuration lives rather than in any repository:

`$XDG_CONFIG_HOME/pigeon/config.json`, or `~/.config/pigeon/config.json`

```json
{
  "namespaces": {
    "clients": { "private": true }
  }
}
```

**This is deliberately not a project setting.** A `.claude/pigeon.json` arrives with a
`git clone`, so if privacy lived there a cloned repository could mark its own namespace
private and hide its sessions from you, or mark yours public. A repository may say which
namespace its sessions belong to; only you may say which namespaces are private.

A private namespace is:

- **Invisible from outside, to Claude.** It is not listed by `pigeon namespaces`, not
  included in `ls --all-namespaces`, and not addressable with `-n`. The MCP tools cannot
  see or reach it at all.
- **Entirely normal from inside.** Its members see each other, message each other, and
  see the other (non-private) namespaces around them, exactly as any session does.
- **Sealed against machine-wide topics, both ways.** Its sessions do not join `@all`, do
  not receive any `@topic`, and cannot publish to one. A private namespace that could
  still broadcast to `@all` would publish exactly what it was made private to keep in.

The escape hatch is your own terminal. The rule keys on `CLAUDE_CODE_SESSION_ID`, which
Claude Code injects into everything it spawns -- the MCP server and the agent's shell
alike -- and which a terminal you opened yourself does not have. So `pigeon ls -n clients`
works for you and not for the agent sitting in another window.

**What this is not.** It is a boundary against ordinary reach, not a sandbox. Anything
that can run `env -u CLAUDE_CODE_SESSION_ID pigeon ls -n clients` can still look, and
pigeon has no privilege with which to stop it -- the state directory is yours and every
process running as you can read it. What it buys is that a private namespace never lands
in a model's context by accident, which is the thing actually worth having. If you need
a real boundary, run those sessions as a different user.

## Opting a session out

Set `PIGEON=0` in its environment. Intended for programmatically driven sessions
(claude-p, pupptyeer, CI) where a driver already owns the conversation. The launcher knows
how it started the session, so it declares it — pigeon does not try to infer it.

A whole project can opt out with `"enabled": false` in its `.claude/pigeon.json`. The
environment outranks the file, since a launcher knows more about how a session started
than a file that arrived with a clone does.

## How it works

Claude Code plugins can declare background monitors that the host starts at session start,
with no model involvement. Every line such a monitor prints to stdout is delivered to that
session as a `<task_notification>`, which wakes it if idle.

Monitors themselves are documented: Claude Code's [plugins
reference](https://code.claude.com/docs/en/plugins-reference) describes declaring them, that
each runs "for the lifetime of the session", and that every stdout line reaches the model as
a notification. **The identity half is not.** That `CLAUDE_CODE_SESSION_ID` is injected into
a monitor process appears nowhere in those docs, and pigeon's addressing rests entirely on
it. It is observed, not promised: verified end to end against Claude Code 2.1.218, and
requiring 2.1.105 or newer for the session id. A future release could drop it, and the
failure would be silent. That is what `pigeon doctor` is for: it checks each assumption
separately and warns when your Claude Code is newer than the version this was tested against.

**A monitor that dies mid-session does not come back on its own.** Claude Code arms monitors
at session start and on plugin reload. That is the documented list -- nothing supervises a
monitor or respawns one whose process is gone. Two consequences, both observed in Claude Code
2.1.220 on 2026-07-28, in two different projects, after the plugin's own "monitor stopped"
notification fired:

- **A session that crosses a restart gets a monitor, under a new address.** A relaunched or
  resumed session is a new process hitting session start, so it arms correctly -- but
  `--resume` mints a fresh session id rather than restoring the old one, so the new monitor
  registers as a different session. Observed silently, mid-conversation: the transcript kept
  going as one continuous conversation while the old id's registry entry disappeared, and
  anyone still holding that address could no longer reach the session.
- **A session that does not cross one stays deaf forever.** After an idle-then-cache-restore
  (no `/clear`, no relaunch, no visible interruption) the session id never changed and no
  monitor was ever rearmed. The session came back fully deaf, indistinguishable from a
  healthy one until someone ran `pigeon doctor`.

Neither case is announced anywhere in the conversation, and the status line's widgets
describe whatever process is current, not necessarily the one you last knew the address of.
Treat a resumed or previously-idle session's identity as unverified until `pigeon
doctor`/`pigeon whoami` confirms it, especially before relying on it to receive. Restarting
the session is the recovery that is known to work.

The session name in `pigeon ls` leans on one more observation of the same kind: Claude Code
writes a per-session index under its own config directory
(`~/.claude/sessions/<pid>.json`, relocated by `CLAUDE_CONFIG_DIR`), and pigeon reads the
`/status` name from there. It is keyed by the claude pid and verified against the session id
before it is trusted, `pigeon doctor` reports whether the read works, and a release that
moves the file costs the column and nothing else.

pigeon's monitor follows two kinds of source: the session's own inbox spool, and one log
per subscribed topic. It identifies itself from `CLAUDE_CODE_SESSION_ID`, which Claude Code
injects into the processes it spawns. If that variable is missing it **fails loudly** rather
than guessing from the process tree — a wrong guess would deliver another session's mail.

State lives in `~/.claude/pigeon` (`PIGEON_HOME` to relocate), owner-only. Each
[namespace](#namespaces) gets its own copy of the tree, under `namespaces/<ns>/`:

```
sessions/<id>.json     registry entry
inbox/<id>.ndjson      direct messages
topics/<topic>.ndjson  topic log for this namespace
cursors/<id>.json      per-session read offsets
locks/<id>.lock        liveness
payloads/<msg>.txt     bodies too long to inline
```

Alongside it, `shared/` holds the logs and payloads of the machine-wide `@` topics, plus
their locks: two namespaces compacting one shared log under their own locks would rewrite
it from under each other.

Notifications are clipped at about 512 characters by Claude Code, so messages over ~300
are written to `payloads/` and the recipient gets a path instead.

## Limits

- Interactive sessions only. Headless `claude -p` arms no monitors.
- Receiving is Unix only. The binary builds and sends from Windows, but Claude Code does
  not arm plugin monitors there, so a Windows session cannot receive.
- PID-reuse detection needs Linux `/proc`; elsewhere a recycled PID can make a dead
  session look alive until something prunes it.
- A monitor that dies mid-session is not respawned. A session that restarts gets a new one
  under a new address; one that only idles stays deaf. Silently, either way -- see
  [How it works](#how-it-works).
- `pigeon prune` does not compact topic logs on Windows. Compaction replaces a log by
  rename, which Windows refuses while any process still holds it open. Forgetting dead
  sessions still works; only the space reclaim is skipped.
- One machine. No network transport.
- 30 notifications per minute per session; beyond that pigeon reports suppression rather
  than being stopped by Claude Code.
- Namespaces are organisation, not a security boundary. They keep unrelated work out of
  each other's listings and broadcasts; they do not stop anything that can write the state
  directory from reaching across, and `-n` exists precisely so you can.
- Unauthenticated: anyone who can write your `~/.claude/pigeon` can inject text into your
  sessions. See [SECURITY.md](SECURITY.md).

## Prior art

pigeon builds on work by others, and depending on your needs one of them may suit you
better — encryption, non-Claude agents, or a broader context server.
[docs/ALTERNATIVES.md](docs/ALTERNATIVES.md) compares them honestly and collects the
mechanism's many sharp edges.

## Licence

MIT.
