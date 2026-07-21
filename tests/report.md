# CLI Test Run — 2026-07-21 (v0.2.0)

## Summary

| Test | Status | GIF |
|------|--------|-----|
| Archive-backed JSON listing | PASS | [view](output/list-archive.gif) |
| Outcome and gold-tag workflow | PASS | [view](output/outcome-tag.gif) |
| Read-time tool-output filtering | PASS | [view](output/get-filter.gif) |
| Push dry-run and pipeline tag query | PASS | [view](output/push-pipeline.gif) |

## All recordings

### Archive-backed JSON listing

![Archive-backed JSON listing](output/list-archive.gif)

### Outcome and gold-tag workflow

![Outcome and gold-tag workflow](output/outcome-tag.gif)

### Read-time tool-output filtering

`tracer get --tool-output=none` and `--turns=user,agent` against the fixture archive; archive bytes are untouched by reads.

![Read-time tool-output filtering](output/get-filter.gif)

### Push dry-run and pipeline tag query

`tracer push demo --dry-run` lists pending transcripts without connecting; `tag <id> wiki:compiled` then `list --json --tag '!wiki:compiled'` shows the unprocessed-set query the digest pipeline uses. The full push→receive transfer path is covered by the pipe-based e2e test in `pkg/transfer`.

![Push dry-run and pipeline tag query](output/push-pipeline.gif)
