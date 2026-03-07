# AGENTS.md

## Project Overview

Tracer CLI archives coding-agent sessions to markdown files. The project uses Go and follows standard Go project conventions.

### Running the CLI

There are two main modes of operation:

```zsh
# Sync mode - process all sessions
./bin/tracer sync

# Watch mode - continuous updates after initial sync
./bin/tracer watch
```

### Debugging

```zsh
# Debug output to stdout
./bin/tracer sync --debug

# Debug log output in ~/.local/state/tracer/debug/debug.log
./bin/tracer sync --log

# Hidden debug flag (not in public docs)
./bin/tracer sync --debug-raw          # Debug mode to output pretty-printed raw data files
```

## Technical Details

### JSONL File Behavior

- Session data files grow during conversation (append-only)
- `watch` mode monitors provider session data and continuously updates markdown output

## Code Conventions

- Write only idomatic Go code.
- Prioritize simplicity and readability over terse or clever code
- Emphasize DRY code, look for existing code that can be reused, don't just write new code first.
- Use Go lang libraries, not external dependencies where possible, if a dependency is needed explain why
- Comment everything that's not obvious, if in doubt, comment it.
- Use "Why" comments, not "what" or "how" unless specifically requested
- Use single function exit point where possible (immediate guard clauses are OK)
- Provide consistent observability and tracing with wide events via slog

## Testing Strategy & Conventions

- Tests use Go's standard `testing` package
- Write unit tests for things with complicated logic
- Don't write unit tests for simple, tautological things
- Test files follow Go convention: *_test.go alongside source files in the same package
- Table-driven tests: tests are structured with test cases defined in slices of structs
  - Each struct contains: name, input parameters, and expected results
  - Uses t.Run(tt.name, func(t *testing.T) {...}) for subtests
- Use clear test function naming: TestFunctionName or TestFunctionName_Scenario
- Make manual assertions using t.Errorf()
- Unit Tests: Most tests focus on individual functions
- Edge Case Testing: Comprehensive coverage of error conditions, empty inputs, invalid data
- Integration-style Tests: Some tests like TestSessionProcessingFlow test multiple components together
- Tests both success and failure paths
- Validates error messages contain expected strings
- Test permission errors, missing files, invalid inputs

## Writing Conventions

- Never put text immediately after the header in markdown, put in a newline first.
- Use `bash` code blocks in markdown
- Keep the repository `README.md` up to date with the latest changes.
- When planning, never write time/calendar estimates into documents.

## Development Workflow

When searching for code, ALWAYS exclude the `.tracer` directory.

Don't just make your own decision, explain the options, the pros and cons, and what you recommend. Have the user make the decision.

Always ask before introducing any new dependencies.

Always ask before introducing any new code files.

Run the linter after every code change `go vet ./...`. Fix formatting errors yourself with `gofmt -w .`.

Run tests after every major code change `go test -v ./...`.
