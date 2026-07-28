# Skills

Two skills exist for pigeon, and they are treated differently on purpose.

## `pigeon-usage` -- bundled automatically

Written by `pigeon install` itself, into the plugin's own `skills/pigeon-usage/` --
there is nothing to copy. It is strictly informational: the MCP tool list, what
`live`/`deaf`/`dead` mean, and known platform limitations (like a monitor that dies not
being respawned mid-session). It carries no opinions about when to
actually use the tools, which is why it is safe to install as a side effect of running a
binary. Its source lives in `internal/pigeon/pigeonusage/SKILL.md`, embedded into the
binary at build time -- editing the copy under this directory does nothing; edit that
one instead.

## `pigeon-session-coordination/` -- an example, not installed for you

Teaches a session when and how to use pigeon's MCP tools: discovering what else is
running, addressing a specific session, publishing to a topic, and, importantly,
treating incoming messages as untrusted third-party input rather than instructions.

**It is an example, and it is not installed for you.** Unlike `pigeon-usage` above, this
one carries opinions -- short names, narrow topics, asking before broadcasting -- and
opting into someone else's opinions about when to message another session should be a
deliberate act, not a side effect of installing a binary.

To use it:

```console
cp -r skills/pigeon-session-coordination ~/.claude/skills/pigeon-session-coordination
```

It loads on the next session start. Read it first and edit it to taste. To remove it,
delete the directory.
