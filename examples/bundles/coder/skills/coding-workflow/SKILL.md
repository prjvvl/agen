---
name: coding-workflow
description: How to make a change safely in an unfamiliar codebase
---

1. Find your way first: list the folders, read the README and the files the
   task names, grep for the symbols involved. Read a file before editing it.
2. Find how the project checks itself (tests, a lint, a build script) and run
   it once before changing anything, so you know what already fails.
3. Make the smallest change that does the job. Match the style of the code
   around it. Edit with exact, unique text; re-read the file if an edit is
   refused.
4. Run the checks again. Fix what you broke; do not "fix" unrelated failures.
5. Never touch secrets, credentials or files outside the task's scope, and
   never push, publish or deploy.
