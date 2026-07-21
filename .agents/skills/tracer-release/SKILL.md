---
name: tracer-release
description: Cut and verify a tracer release. Use when bumping the version, shipping a hotfix, or asked to release tracer. Covers the flake-versioned release pipeline, pre-flight checks, the review gate, post-release verification, and the fleet rollout.
---

# Tracer Release

Bumping `version` in `flake.nix` on main IS the release: the Release workflow
triggers on the `flake.nix` path filter, re-runs the Go checks, builds via nix,
asserts the embedded version, and creates the tag + GitHub release atomically
(idempotency is keyed on release existence, so interrupted runs self-heal and
non-version flake edits no-op cleanly).

## Pre-flight (on the release branch)

```bash
gofmt -l .            # must be empty (excluding .tracer/)
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./... > /tmp/t.out 2>&1; echo "exit=$?"   # check the CODE, never pipe to a filter
nix eval --raw .#tracer.version   # must match the intended X.Y.Z
```

- CHANGELOG.md gets a section for the new version (blank line after headers).
- `vendorHash` in flake.nix must be refreshed whenever go.mod changed (CI's
  nix-build job catches drift, but catch it locally first).
- Any CLI flag/command change requires updating `pkg/cmd/skill.go` in the same
  change; the drift-guard test scopes flags per command line, so a flag
  mentioned on a line containing `tracer <subcommand>` must belong to that
  subcommand.

## Ship

1. Branch, commit, push, open a PR onto main.
2. Gate on CI AND the review bot (see the review-bot-gate skill): triage every
   comment, fix true positives, dismiss false positives with cited evidence.
3. Merge. Then watch the release run **by run ID**, never `gh run list --limit 1`
   (a just-merged push races the previous run and you will watch the wrong one):

```bash
RUN=$(gh run list --workflow Release --branch main --limit 1 --json databaseId,headSha \
  --jq '.[] | select(.headSha == "'"$(git rev-parse HEAD)"'") | .databaseId')
gh run view "$RUN" --json status,conclusion
```

## Verify the release

```bash
gh release view vX.Y.Z --json tagName,assets   # tracer-linux-amd64 + SHA256SUMS
```

Download the asset (or use `result/bin/tracer` from `nix build`) and confirm
`NO_COLOR=1 tracer version` prints exactly `tracer X.Y.Z`.

PATH gotcha: bare `tracer` resolves to the older home-manager binary in
`/etc/profiles`. Always verify with an explicit path (`./bin/tracer`,
`result/bin/tracer`, or the downloaded asset).

## Fleet rollout

1. In ~/dotfiles: `nix flake update tracer` (leave uncommitted for review
   unless told otherwise).
2. Rebuild order matters for push-protocol changes: the receiver (sietch)
   must run a tracer >= the sender's protocol before jacurutu pushes.
3. Post-rollout end-to-end proof:
   - `systemctl --user start tracer-sync.service` on jacurutu; check
     `journalctl --user -u tracer-sync` for the `Archive push complete` wide
     event (note: no-op pushes with nothing pending do not emit the summary).
   - Annotation round-trip: tag a session `wiki:compiled` on sietch, tag the
     same session `gold` on jacurutu, push again, confirm sietch's copy has
     BOTH tags (union merge) and only the dirtied file transferred.

## Recovery

- Tag exists but release missing (interrupted run): re-run the workflow; the
  release-existence check repairs it.
- Failed push runs never advance the sender cursor; they retry naturally.
