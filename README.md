# gh-triage

Which PRs need my action, which do I owe a review on, and who should I poke?

`gh-triage` computes that from GitHub's raw data (required checks on the head
commit, review state per commit, merge queue state) instead of trusting the
inbox's labels. Every PR is shown with the reason it landed where it did.

## Setup

```bash
gh auth refresh -s read:org -s repo
gh extension install dbraley/gh-triage
```

From a checkout: `go build && gh extension install .`

Config is optional. Copy `config.example.toml` to `config.toml` (gitignored) or
`~/.config/gh-triage/config.toml`. Scope is every open PR you authored, were
asked to review (directly or via a team), or have reviewed; the config
*excludes* repos and orgs from that.

## Usage

```bash
gh triage                      # grouped terminal report
gh triage --markdown           # Obsidian Tasks for a daily note
gh triage --show-bots          # expand the Automated section
gh triage --explain <PR url>   # the decision trail for one PR
gh triage --dump testdata/local   # also save raw responses as fixtures
gh triage --from testdata      # classify fixtures, no network
```

## Buckets

PRs you authored, first match wins:

| Bucket | Means |
| --- | --- |
| ACT NOW | A required check failed on the head commit, merge conflict, changes requested on the current commit, unresolved threads with activity since your last push, ejected from the merge queue, or nobody has been asked to review. |
| IN MERGE QUEUE | Queued; shows position and ETA. |
| READY TO ENQUEUE | Approved, required checks green, no conflicts. |
| POKE | Required checks green, no approval. Names who is holding it, and says RE-REVIEW if they reviewed an earlier commit. |
| WAITING ON CI | A *required* check is queued or running on the head commit. Optional, skipped, neutral and superseded runs never count. |
| DRAFT | Draft with nothing else going on. |

PRs by others: REVIEW REQUESTED (of you directly), RE-REVIEW (you reviewed,
the author pushed), REPLY OWED (someone answered in a thread you commented
on), TEAM REVIEW (requested of your team only).

Bot PRs (Dependabot, Renovate, ...) get the same rules in a collapsed
Automated section. A bot PR that isn't waiting on your review is treated as
yours, so an approved one shows as READY TO ENQUEUE. Bots are never on the
poke list; a human sitting on a bot PR is.

The poke list is everyone holding a PR longer than
`poke_after_business_days`, with a Slack message to paste.

## When a PR is misclassified

1. Capture it: `gh triage --explain <PR url> --dump testdata/local`. The trail
   shows which rule fired; the dump is private and gitignored.
2. Scrub it: `gh triage --scrub testdata/local/<file>.json > testdata/<case>.json`.
   This replaces logins, orgs, repos, teams, titles, check names and commit
   ids with placeholders. Read the result before committing.
3. Add a row to the table in `internal/classify/classify_test.go` with the
   bucket it should be in, run `go test ./...`, and fix `classify.go` until
   it passes.

`gh triage --explain testdata/<case>.json` replays a fixture offline.

## Known limits

- GitHub does not expose push times. "Last push" is the head commit's date
  or the latest force-push, whichever is later.
- A required check that has not reported at all is invisible to the API. If
  GitHub says BLOCKED on an otherwise ready PR, the reason line says so.
- Whether a merge queue removal was an ejection is inferred: it was not you,
  and you have not pushed since. The removal reason is shown.
