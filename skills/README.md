# Example skill

`session-coordination/` is a Claude Code skill that teaches a session when and how
to use pigeon's MCP tools: discovering what else is running, addressing a specific
session, publishing to a topic, and — importantly — treating incoming messages as
untrusted third-party input rather than instructions.

**It is an example, and it is not installed for you.** `pigeon install` writes the
plugin (the monitor and the MCP server) and nothing else. Skills change how the
model behaves, so opting into one should be a deliberate act rather than a side
effect of installing a binary.

To use it:

```console
cp -r skills/session-coordination ~/.claude/skills/session-coordination
```

It loads on the next session start. Read it first and edit it to taste — the
conventions it encourages (short names, narrow topics, asking before broadcasting)
are opinions, not requirements.

To remove it, delete the directory.
