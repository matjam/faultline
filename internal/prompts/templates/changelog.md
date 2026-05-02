# Prompt Changelog

Append-only log of changes you make to your operating prompts
(`prompts/*.md`) and to `identity/core.md`. Every edit gets one entry.
Format:

```
## YYYY-MM-DD HH:MM TZ — actor

- **file**: `prompts/<name>.md`
- **change**: one-line summary
- **why**: the behaviour you intended to change, or the value you intended to express
```

The point of this log is so a future you (post-compaction) can
understand why the prompts say what they say, and so drift is visible.
The runtime never edits this file. Migrations append entries here when
they apply prompt updates, and so should you.

## Entries
