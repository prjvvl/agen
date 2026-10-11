---
name: review-checklist
description: What to check in a pull request, by kind of change, before writing the review
---

Check what applies to the change; skip the rest.

- **Behaviour:** edge cases (empty, missing, very large, concurrent), error
  paths that are swallowed or logged without being handled, off-by-one
  limits, changed defaults.
- **Tests:** new behaviour without a test; tests that only cover the happy
  path; tests changed to match a bug instead of fixing it.
- **Security:** user input reaching SQL, shell commands, file paths, URLs or
  HTML without validation or escaping; secrets in code, logs or errors;
  permission checks that were removed or moved after the action.
- **Data:** schema migrations that are not reversible or lock large tables;
  changed formats without a reader for the old one.
- **APIs:** removed or renamed fields, endpoints or flags that callers still
  use; changed meaning of an existing field.
- **Dependencies:** new packages (are they maintained, is the version
  pinned), large upgrades hidden in an unrelated change.

Each finding names the file and line and says what goes wrong, not just
that something looks off.
