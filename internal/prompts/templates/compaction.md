Context compaction. Anything not in a memory file or in your summary will be permanently lost. Before responding:

1. Persist anything you want to survive compaction to memory files. Be deliberate — pick filenames you can find again. A separate "snapshot" file (full current state) and "summary" file (compact for fast reload) is one pattern; develop whatever convention works for you and stick to it.

2. Verify your writes actually landed by reading or listing them.

3. Then respond with a summary of your current state. This summary plus your recent memory files are what your context will be rebuilt from.
