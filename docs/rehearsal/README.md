# Rehearsal log

The record of the first real-host rehearsal of Xenos (the VPS runbook, `docs/test-host-runbook.md`): what was done, what went wrong, and how it was fixed. It is written **as the work happens**, so nothing depends on memory. Read it before you set up the paid host: every problem found here is one you will not meet again.

## Files

| File | What it is |
|---|---|
| `log.md` | The curated, chronological log: one entry per step, with every problem and its fix. Start here. |
| `issues.md` | One row per problem found, in a table: symptom, root cause, fix, where the fix lives (commit or doc), and whether the real-server runbook needed changing. |
| `transcript.md` | Raw record of the commands run and their (trimmed) output, in order, for anyone who needs the exact detail. |

## Rules for the entries

1. **Every step gets an entry**, including the ones that went fine ("ok, nothing to report" is information).
2. **Every problem gets the same five things:** what was tried (exact command), what happened (exact output, trimmed to the relevant lines), the root cause (and how it was established, not guessed), the fix, and how the fix was verified. A fix that changed the code or a runbook names the commit or file.
3. **Wrong turns stay in.** A theory that was tried and rejected is recorded with the reason: the next person will have the same theory.
4. **No secrets, ever.** API tokens, passwords, private keys and the VPS's real IP are replaced with `<redacted>`/`<vps-ip>` before anything is written. Customer-looking data is synthetic here anyway.
5. **Runbook corrections are made in the runbook too**, not only logged: when a step in `test-host-runbook.md` or `host-setup-runbook.md` turns out wrong, the runbook is fixed in the same session and the log entry says so.
6. **Each entry has a verdict:** `PASS`, `PASS (after fix)`, `FAIL (open)`, or `NOT TESTED (and why)`.

## Entry template (`log.md`)

```
### Step N: <title>   <verdict>
Date/time (UTC) · duration
Goal: …
What was done: commands (redacted), in order
Result: what the Check showed
Problems: (none) | numbered list; each links to its row in issues.md
Notes for the team: anything surprising, anything the real server will do differently
```

## Row template (`issues.md`)

`| ID | Step | Symptom | Root cause | Fix | Verified by | Where fixed | Real-server impact |`
