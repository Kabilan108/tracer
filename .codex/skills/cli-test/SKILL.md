---
name: cli-test-vhs
description: Define, run, and review visual CLI test cases using VHS tape files. Use when building or modifying CLI tools to verify behavior through recorded terminal GIFs and golden output files.
---

# CLI Test Suite with VHS

Use VHS tape files as lightweight, visual test cases for CLI tools. Each tape defines a repeatable interaction that produces a GIF for human review and a golden text file for diffing.

## When to use

- After building or modifying a CLI tool, to verify behavior
- When the user asks to test, demo, or validate CLI commands
- When the user says "run the tests", "check the CLI", "record a demo"

## Workflow

1. **Define** tape files — one per test case
2. **Setup** prerequisites outside VHS (daemons, test data)
3. **Run** tapes in parallel where possible
4. **Report** — generate a markdown summary with embedded GIFs
5. **Review** — user scrolls the report, gives feedback
6. **Fix & rerun** failing cases

## Directory structure

```
tests/
  tapes/           # .tape files (test definitions)
  output/          # generated GIFs and golden .txt files
  report.md        # test run summary (regenerated each run)
```

## Writing tape files

### Naming

Name tapes after the behavior they test: `help-flag.tape`, `create-project.tape`, `daemon-status.tape`.

### Template

```
Require <cli-binary>

Output tests/output/<name>.gif
Output tests/output/<name>.txt

Set Shell "bash"
Set Width 1200
Set Height 600
Set FontSize 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 80ms
Set CursorBlink false

# --- test interaction ---
Type "<command>"
Enter
Wait /<expected-output-pattern>/
Sleep 1s
```

### Key rules

- **`Wait /regex/` over `Sleep`** for command output. Use `Sleep` only for pacing the visual recording (short durations — 500ms to 2s).
- **Always dual output**: both `.gif` (visual review) and `.txt` (diffable golden file).
- **`Require`** declares what must be in PATH. Add one for the CLI under test and any other tools used in the tape.
- **Keep tapes focused**: one behavior per tape. A tape that tests `mycli create` should not also test `mycli delete`.
- **Use `Hide`/`Show`** to skip noisy setup that the user doesn't need to see in the GIF, but that must run in the terminal session (e.g., setting env vars, cd-ing into a temp dir).

### Handling setup and teardown

VHS records a single terminal session. For prerequisites that live outside that session:

**Daemons / background services**: Start in a tmux pane before running the tape. Kill the pane after.

If already inside tmux (check `$TMUX`), use a new pane in the current window. Only create a new session if not inside tmux.

```bash
if [ -n "$TMUX" ]; then
  # inside tmux — split a new pane, run daemon there
  tmux split-window -d -P -F '#{pane_id}' "mycli serve --port 9999"
  DAEMON_PANE=$(!!)  # capture pane id
else
  # not inside tmux — create a detached session
  tmux new-session -d -s test-daemon "mycli serve --port 9999"
fi
sleep 2  # wait for daemon to be ready

# run the tape
vhs tests/tapes/client-status.tape

# teardown
if [ -n "$DAEMON_PANE" ]; then
  tmux kill-pane -t "$DAEMON_PANE"
else
  tmux kill-session -t test-daemon
fi
```

**Temp directories / test fixtures**: Create before, clean up after. Use `Hide`/`Show` inside the tape if the tape itself needs to cd or set env vars.

**Shared state across tapes**: If two tapes need the same daemon, group them and run sequentially within that group. Independent groups run in parallel.

## Running tapes

### Parallel execution

Run independent tapes concurrently. Group tapes that share state.

```bash
# independent tapes — run in parallel
vhs tests/tapes/help-flag.tape &
vhs tests/tapes/version.tape &
vhs tests/tapes/invalid-args.tape &
wait

# tapes requiring a daemon — run sequentially within the group
if [ -n "$TMUX" ]; then
  DAEMON_PANE=$(tmux split-window -d -P -F '#{pane_id}' "mycli serve")
else
  tmux new-session -d -s daemon "mycli serve"
fi
sleep 2
vhs tests/tapes/client-connect.tape
vhs tests/tapes/client-status.tape
if [ -n "$DAEMON_PANE" ]; then
  tmux kill-pane -t "$DAEMON_PANE"
else
  tmux kill-session -t daemon
fi
```

### Checking golden files

After running, diff the `.txt` output against the previous run:

```bash
# diff each golden file against its committed version
for f in tests/output/*.txt; do
  git diff --no-index -- "$f.bak" "$f" 2>/dev/null && echo "PASS: $f" || echo "FAIL: $f"
done
```

Or simply use `git diff tests/output/` if the golden files are tracked.

## Generating the report

After each test run, generate `tests/report.md`. This is the primary review artifact.

### Report format

```markdown
# Test Run — <date> <time>

## Summary

| Test | Status | GIF |
|------|--------|-----|
| help-flag | PASS | [view](output/help-flag.gif) |
| create-project | FAIL | [view](output/create-project.gif) |

## Failed tests

### create-project

**Expected output (golden):**
\```
Usage: mycli create <name>
Project "demo" created.
\```

**Actual output:**
\```
Usage: mycli create <name>
Error: missing required argument
\```

**Diff:**
\```diff
- Project "demo" created.
+ Error: missing required argument
\```

## All recordings

### help-flag
![help-flag](output/help-flag.gif)

### create-project
![create-project](output/create-project.gif)
```

### Report rules

- Summary table at top with pass/fail status for every tape
- Failed tests section shows the golden diff immediately — no need to open files
- All GIFs embedded at the bottom for visual scroll-through
- Regenerate the report from scratch on each run — it's ephemeral, not version-controlled

## Agent behavior

### Defining tests

- Read the CLI's help output and source to understand available commands
- Write one tape per distinct behavior (subcommand, flag combination, error case)
- Start with the happy path, then cover error cases and edge cases
- Ask the user if the test set looks reasonable before running

### Running tests

- Identify which tapes share state and group them
- Run independent groups in parallel using background processes
- Capture exit codes to determine pass/fail
- Generate the report after all tapes complete

### After review

- When the user says a test is wrong, fix the CLI code (not the tape) unless the tape itself has a bug
- Rerun only the failing tapes, not the full suite
- Regenerate the report with updated results

### When not to record

- Unit tests and logic tests belong in the project's normal test framework
- VHS tapes test the CLI's user-facing behavior — what the human sees and types
- Don't use tapes for testing library code, internal APIs, or non-interactive programs
