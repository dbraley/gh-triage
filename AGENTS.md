# AGENTS.md

`gh-triage` is a Go CLI and `gh` extension that sorts open PRs into buckets
(act now, ready to enqueue, poke, ...) computed from GitHub's raw data. See
`README.md` for the buckets and usage.

## This repo is public; real PR data is private

- Real dumps live only in `testdata/local/` (gitignored), as does `config.toml`.
  Never commit, paste into a commit message, or quote from either.
- A fixture may be committed only after `gh triage --scrub`, and only after
  reading the scrubbed file for leftover org, repo, login, branch, team or
  check names.
- If the query gains a field that carries private text, extend
  `internal/fetch/scrub.go` and `scrub_test.go` in the same change.
- Use placeholders (`org-1/repo-1`, `user-3`) in tests, docs and examples.

## Commands

```bash
go test ./...                          # no network; runs against testdata/*.json
go build && gh extension install .     # local install (a link to this checkout)
gh triage --from testdata              # classify the committed fixtures offline
gh triage --explain testdata/<case>.json   # decision trail for one fixture
```

Release: push a `v*` tag; `.github/workflows/release.yml` builds the binaries.
Commits to `main` alone do not reach installed users.

## Layout and invariants

- `internal/fetch`: the GraphQL queries, the types they decode into, and the
  only code that talks to GitHub. Also the scrubber.
- `internal/classify`: pure functions of a `fetch.PR` plus `Options`. No
  network and no clock; "now" comes from `Options.Now`, which fixtures supply
  via `fetchedAt`.
- `internal/render`: terminal, markdown and explain output.
- `main.go`, `config.go`: flags, config loading, wiring.

Rules that are easy to break:

- Checks are judged on the head commit only, and only required ones count
  (per-PR `isRequired`, which is why each PR is fetched individually).
  Reruns are deduped by check name, latest wins.
- Authored buckets are first match wins; `AuthoredOrder` in `types.go` is
  display order, not evaluation order.
- Every decision appends to `Result.Trail` so `--explain` shows why.
- Thread state is decided by the latest *human* comment; bot comments are
  ignored.
- Idle time is business time (weekends excluded).
- Dependencies: `go-gh/v2` and one TOML library. Otherwise stdlib.

## Changing classification

Tests first. For a misclassified PR:

1. `gh triage --explain <PR url> --dump testdata/local`
2. `gh triage --scrub testdata/local/<file>.json > testdata/<case>.json`
3. Add a row to the table in `internal/classify/classify_test.go` with the
   expected bucket, then change `classify.go` until it passes.

A new fixture changes the counts asserted in `main_test.go`
(`TestRunFromFixtures`); update them. If behaviour changes, update the bucket
table in `README.md` and `config.example.toml` when a config key is involved.
