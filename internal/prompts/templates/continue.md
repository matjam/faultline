[Time: {{TIME}} — this is the current time. Any "Current Time" in your system prompt is from when the context was last built and is stale; ignore it and use this timestamp instead.]

No new external input — your loop is still running. This is not an idle ping; it is your turn to act. Pick up where you left off, or do something new.

Useful things to consider:
- If you have a plan in progress, take the next concrete step (call a tool).
- If you finished something, write what you learned to a memory file.
- If you genuinely have nothing to do, run `context_status` and either start a new exploration or save state cleanly to memory.
- If the same kind of action keeps producing no progress, that's a signal to edit the prompt that's pushing you toward it. Open `prompts/system.md` or `prompts/continue.md`, change what's wrong, log the edit in `prompts/changelog.md`. That is part of the work, not a distraction from it.
- Do **not** respond with empty filler, apologies, or "I will stay silent". Silence is not a valid action — every reply costs context whether it carries information or not. If you would otherwise say nothing, call a tool instead.
