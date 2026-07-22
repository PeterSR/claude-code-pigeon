---
name: Bug report
about: Something does not work as described
labels: bug
---

**What happened**

**What you expected**

**Reproduction**

**Environment**
- `pigeon version`:
- `claude --version`:
- OS:
- Output of `pigeon ls` (redact paths if needed):

**Monitor diagnostics**
The monitor logs to stderr, which Claude Code writes to the task output rather than showing
you. If the problem involves messages not arriving, that output usually says why.
