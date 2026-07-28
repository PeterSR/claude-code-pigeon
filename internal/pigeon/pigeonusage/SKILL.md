---
name: pigeon-usage
description: Orientation to pigeon -- the MCP tools it exposes, what live/deaf/dead mean, and known platform limitations around session resume. Use when you need a quick reference for pigeon's tools or status meanings, or want to check whether this session's own reachability can be trusted after a resume or an idle period.
---

# Using pigeon

`pigeon` lets Claude Code sessions on this machine see each other and pass messages,
even when the recipient is sitting idle at the prompt.

## Tools

- `list_sessions` -- what else is running: address, status, cwd, description.
- `send_message(to, text)` -- direct message. `to` accepts a declared name, a session
  id (or its prefix), or the basename of a working directory.
- `publish(topic, text)` / `subscribe(topic)` / `unsubscribe(topic)` -- broadcast to
  whoever is listening on a topic. Every session starts subscribed to `all`.
- `list_topics` / `list_namespaces` -- what topics and namespaces exist, and how many
  live sessions are in each.
- `whoami` -- this session's own identity: session id, namespace, pid, declared name,
  and the address others use to reach it.
- `set_identity(name, description)` -- declare a short name and what this session is
  working on, so others can find and address it.

## Status meanings

- `live` -- a monitor is listening; a message arrives in about a second.
- `deaf` -- the process is running, but nothing is listening. A message will sit
  unread; treat this as "will probably not arrive," not "will arrive eventually."
- `dead` -- the process is gone.

## Known limitation: reachability after a resume or a long idle gap

A monitor can die silently, and nothing respawns one mid-session. Observed in Claude
Code 2.1.220 on 2026-07-28: a session that restarted came back with a monitor under a
brand-new address, no announcement, while the old address went unreachable; a session
that only idled came back with no monitor at all and was simply deaf. Nothing in the
conversation flags either case, and the status line's widgets describe whatever
process is current, not necessarily the one you last knew the address of.

If this conversation was just resumed, has been idle a while, or you saw a "monitor
stopped" task notification you never followed up on, do not assume you are still the
address anyone last saw. Call `whoami` before relying on it, and if it looks off, or
the user asks whether you can still receive, say so plainly rather than assuming
messages are getting through.

## More

This skill is strictly informational and carries no opinion about *when* to actually
reach for these tools. For workflow conventions on that -- when coordinating is worth
the cost, how to treat what arrives -- see the separate `pigeon-session-coordination`
skill, an opt-in example rather than something installed automatically.
