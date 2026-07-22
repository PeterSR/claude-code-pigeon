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

## Monitors are unsandboxed

A plugin monitor runs arbitrary code for the lifetime of your session, at the same trust
level as hooks, with no per-run approval. Consent happens once, at install time. That is
true of pigeon and of every other plugin shipping a monitor. Read what you install.

## Reporting

Open an issue. If you would rather not do so publicly, say so in a minimal issue and we
will find another channel.
