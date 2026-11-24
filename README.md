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
gh triage --show-snoozed       # expand the Snoozed section
gh triage snooze <PR url> [--for 3d]   # hide a PR until something happens
gh triage unsnooze <PR url>    # bring it back now
gh triage --explain <PR url>   # the decision trail for one PR
gh triage --dump testdata/local   # also save raw responses as fixtures
gh triage --from testdata      # classify fixtures, no network
```

## Buckets

PRs you authored, first match wins:

| Bucket | Means |
| --- | --- |
| ACT NOW | A required check failed on the head commit, merge conflict, changes requested on the current commit, unresolved threads with human activity since your last push, ejected from the merge queue, or nobody has been asked to review. |
| IN MERGE QUEUE | Queued; shows position and ETA. |
| READY TO ENQUEUE | Approved, required checks green, no conflicts. |
| WAITING ON STACK | Approved and green, but it targets another PR's branch rather than the default branch. |
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

## Snoozing

`gh triage snooze <PR url>` hides a PR from the report, and from the poke
list, until the first of:

- a new push;
- a new comment or review by someone other than you or a bot;
- your review being requested again (you, not a team);
- a change in what it needs: its bucket changes, or, for ACT NOW, a new
  problem appears (a failing check, a conflict, changes requested, new thread
  activity, a merge queue ejection, ...). A problem going away does not wake it;
- the time limit.

The push and bucket/problem wakes apply whoever caused them, including you;
only comments and reviews are limited to other humans.

Snoozed PRs are counted in a collapsed SNOOZED line (`--show-snoozed` lists
them and when they wake). A PR that wakes is shown normally with a note such as
`snooze ended: new comment by user-3`, on the first report after it wakes and
only that one.

`snooze` prints the bucket and reason it snoozed, since the PR may have changed
since you last looked. Snoozing again replaces the old snooze. `<PR url>` is a
PR URL (a trailing slash, `/files`, a query or a fragment are fine), or
`org/repo#123`; `unsnooze` accepts the same forms. A fixture file also works
for offline use, with `--state FILE`.

The limit is `[snooze] default_business_days` (3), or `--for`: whole days
and/or hours (`3d`, `12h`, `1d12h`). A day is 24h. Saturdays and Sundays do not
count for either unit, so `--for 12h` on a Friday evening wakes on Monday.

A snooze is dropped once its PR no longer appears in your report (closed,
merged, excluded by config, or you are no longer involved). This is only
checked on a live report where every lookup succeeded.

Flags: `--for` goes after `snooze`; `--config` and `--state` work on either
side of the subcommand. `--explain` shows a PR's snooze status and never
changes it.

State is a private local file, `$XDG_STATE_HOME/gh-triage/snoozes.json`
(default `~/.local/state/gh-triage/`), or `--state FILE`. It names your PRs;
never commit it. `--from DIR` ignores it unless `--state` is given.

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
- Snooze checks only the 30 most recent conversation comments and the 30 most
  recent reviews on a PR.
- The snooze state file has no lock: two simultaneous runs are
  last-writer-wins.
