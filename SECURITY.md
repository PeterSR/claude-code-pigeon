# Security

pigeon injects text into the context of a running AI agent. Treat it accordingly.

## Trust model

**There is none beyond the filesystem.** Any process running as your user can write to
`~/.claude/pigeon` and therefore inject text into any of your sessions. Sender identity is
recorded but **not authenticated** — a `from` stamp is a claim, not a proof.

State is created `0700`, files `0600`. That is the entire boundary. Do not put pigeon's
state on a shared filesystem.

## Prompt injection

Message text arrives in a `<task_notification>` alongside your own conversation. A hostile
sender would like it to read as an instruction. pigeon:

- strips control characters and flattens newlines, so a sender cannot append what looks
  like a separate turn;
- replaces `<` and `>` with lookalikes, so a sender cannot forge a closing tag and open a
  block of their own;
- constrains self-declared names and topic names to a safe character set at the point they
  are set, and sanitises them again at render time, so a peer cannot spoof another;
- validates session ids before they are used as path components, so a planted registry
  entry or a hostile `CLAUDE_CODE_SESSION_ID` cannot steer file operations outside the
  state directory;
- phrases every notification as a report about an event, never an imperative.

That last point is not cosmetic: waking a session with imperative text makes the model echo
`Human:` blocks or fabricate an entire user message that then persists in the transcript
(anthropics/claude-code#60360).

**Sanitising is not authorisation.** A message can still say something persuasive. Treat
incoming messages as untrusted input from a third party — which is what they are — and do
not let an agent act on one in a way you would not let a stranger's email trigger.

## The socket transport moves the sanitising, and moves the forgery surface

When pigeon delivers over a session's Claude Code inbox socket rather than through its
monitor, everything above still applies — the line is rendered by the same code, with the
same stripping and the same phrasing — but two things are different, and both are worth
knowing before you make `socket` your default.

**The record and the delivery come apart.** On the monitor path, the line a session sees
was produced by pigeon's own process reading pigeon's own spool, so anything that arrived
is on the log. A socket push puts text in front of a session directly. pigeon writes its
record either way, but *anything else that can reach that socket* can put a line there too,
formatted exactly like a pigeon notification, with nothing on any log behind it. Observed
in practice while building this: a `[pigeon #deploys] from sh :: shipped` line arrived in a
session that was not subscribed to `#deploys`, on a machine where no such topic existed.

This is not a new privilege — reaching the socket needs the same access as writing
`~/.claude/pigeon` — but it is a new way to spend it, and one that leaves no trace. **If a
notification matters, confirm it with `pigeon inbox`**, which reads the record rather than
the doorbell.

**Sender attribution comes from Claude Code, not from pigeon.** A socket push is wrapped in
Claude Code's own `cross-session-message` envelope, so the receiving session attributes it
to whatever name the sender supplied. That name is bounded and stripped of the characters
that would break the envelope, and Claude Code re-serializes what it parses and compares it
against what arrived, so a message body cannot forge the end of its own envelope. It is
still a claim, exactly like the `from` stamp on the spool.

## Monitors are unsandboxed

A plugin monitor runs arbitrary code for the lifetime of your session, at the same trust
level as hooks, with no per-run approval. Consent happens once, at install time. That is
true of pigeon and of every other plugin shipping a monitor. Read what you install.

## Reporting

Open an issue. If you would rather not do so publicly, say so in a minimal issue and we
will find another channel.
