package classify

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the DST-gap test must not depend on system tzdata

	"github.com/dbraley/gh-triage/internal/fetch"
)

// load classifies a fixture using the context stored in the fixture itself
// (viewer, teams, fetch time), so results never depend on the wall clock.
func load(t *testing.T, name string, tweak func(*Options)) (fetch.PR, Result, Options) {
	t.Helper()
	f, err := fetch.LoadFixture(filepath.Join("..", "..", "testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Me: f.Viewer, Teams: f.ViewerTeams, Now: f.FetchedAt, PokeAfter: 24 * time.Hour, AllChecksWhenNoneRequired: true}
	if tweak != nil {
		tweak(&o)
	}
	return f.Node, Classify(f.Node, o), o
}

func holders(r Result) string {
	var s []string
	for _, h := range r.Holders {
		w := h.Who
		if h.ReReview {
			w = "re-review:" + w
		}
		s = append(s, w)
	}
	return strings.Join(s, ",")
}

func TestClassifyFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		bucket  Bucket
		reason  string // substring of Result.Reason
		holders string // comma-joined, "re-review:" prefix where applicable
		poked   string // comma-joined poke-list logins for this PR
	}{
		// Known bug: "waiting on CI" while nothing required is pending.
		{"authored_optional_check_pending", Poke, "user-2", "user-2", "user-2"},
		// Carries a conversation comment and a review for the snooze wake tests.
		{"authored_snooze_activity", Poke, "user-2", "user-2", "user-2"},
		{"authored_rerun_superseded", ReadyToEnqueue, "add to the merge queue", "", ""},
		// Known bug: "waiting on other reviewers" while nobody is.
		{"authored_no_reviewers", ActNow, "request a review", "", ""},

		{"authored_rereview", Poke, "re-review", "re-review:user-2", "user-2"},
		{"authored_changes_requested", ActNow, "changes requested by user-2", "", ""},
		{"authored_required_failed", ActNow, "required check failed: test", "", ""},
		{"authored_conflict", ActNow, "merge conflict", "", ""},
		{"authored_thread_activity", ActNow, "1 unresolved thread", "", ""},
		{"authored_ci_pending", WaitingOnCI, "test", "", ""},
		{"authored_draft", Draft, "draft", "", ""},

		{"queue_queued", InMergeQueue, "position 2", "", ""},
		{"queue_kicked", ActNow, "removed from the merge queue", "", ""},
		{"queue_ready", ReadyToEnqueue, "add to the merge queue", "", ""},

		{"review_direct", ReviewRequested, "requested 3d 2h ago", "", ""}, // Fri -> Wed, weekend skipped
		{"review_rereview", ReReview, "pushed since", "", ""},
		{"review_reply_owed", ReplyOwed, "user-5 replied", "", ""},
		{"review_team_only", TeamOnly, "org-1/team-1", "", ""},
		{"review_nothing_owed", Nothing, "", "", ""},

		// Scrubbed real PRs. Stacked on another PR's branch, with unresolved
		// threads whose only post-push comments are from a bot.
		{"real_stacked_bot_threads", Stacked, "land the PR below it first", "", ""},
		{"real_bot_only_approval", ReadyToEnqueue, "approved only by a bot: github-actions", "", ""},

		// Bots: same rules; never poked themselves, but their human reviewer is.
		{"bot_dependabot", TeamOnly, "org-1/team-1", "user-3", "user-3"},
		{"bot_approved_ready", ReadyToEnqueue, "merge it (this repo has no merge queue)", "", ""},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			pr, r, o := load(t, c.fixture, nil)
			if r.Bucket != c.bucket {
				t.Errorf("bucket = %q, want %q\nreason: %s\ntrail:\n  %s", r.Bucket, c.bucket, r.Reason, strings.Join(r.Trail, "\n  "))
			}
			if !strings.Contains(r.Reason, c.reason) {
				t.Errorf("reason = %q, want it to contain %q", r.Reason, c.reason)
			}
			if got := holders(r); got != c.holders {
				t.Errorf("holders = %q, want %q", got, c.holders)
			}
			var poked []string
			for _, p := range PokeList([]fetch.PR{pr}, []Result{r}, o) {
				poked = append(poked, p.Holder.Who)
			}
			if got := strings.Join(poked, ","); got != c.poked {
				t.Errorf("poke list = %q, want %q", got, c.poked)
			}
			if len(r.Trail) == 0 {
				t.Error("empty decision trail")
			}
			if (r.Bucket == ActNow) != (len(r.Causes) > 0) {
				t.Errorf("causes = %v for bucket %q: set exactly for ACT NOW", r.Causes, r.Bucket)
			}
		})
	}
}

// Causes are persisted in the snooze state file: the keys must stay stable and
// carry no volatile text.
func TestActNowCauses(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		tweak   func(*fetch.PR)
		want    []string
	}{
		{"authored_required_failed", nil, []string{CauseChecksFailed}},
		{"authored_conflict", nil, []string{CauseConflict}},
		{"authored_changes_requested", nil, []string{CauseChangesRequested}},
		{"authored_thread_activity", nil, []string{CauseThreads}},
		{"queue_kicked", nil, []string{CauseEjected}},
		{"authored_no_reviewers", nil, []string{CauseNoReviewer}},
		{"queue_ready", func(pr *fetch.PR) { pr.IsDraft = true }, []string{CauseDraftReady}},
		{"authored_required_failed", func(pr *fetch.PR) { pr.Mergeable = "CONFLICTING" }, []string{CauseChecksFailed, CauseConflict}},
		{"authored_ci_pending", nil, nil},
		{"review_direct", nil, nil},
	} {
		t.Run(tc.fixture+"/"+strings.Join(tc.want, "+"), func(t *testing.T) {
			f, err := fetch.LoadFixture(filepath.Join("..", "..", "testdata", tc.fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.tweak != nil {
				tc.tweak(&f.Node)
			}
			o := Options{Me: f.Viewer, Teams: f.ViewerTeams, Now: f.FetchedAt, PokeAfter: 24 * time.Hour, AllChecksWhenNoneRequired: true}
			r := Classify(f.Node, o)
			if !slices.Equal(r.Causes, tc.want) {
				t.Errorf("causes = %v, want %v (bucket %q: %s)", r.Causes, tc.want, r.Bucket, r.Reason)
			}
		})
	}
	// The wire values are persisted; a rename is a breaking change.
	for _, k := range []string{CauseChecksFailed, CauseConflict, CauseChangesRequested, CauseThreads, CauseEjected, CauseDraftReady, CauseNoReviewer} {
		if strings.ToLower(k) != k || strings.ContainsAny(k, " :/") {
			t.Errorf("cause key %q is not a short stable key", k)
		}
	}
	if CauseChecksFailed != "checks-failed" || CauseConflict != "conflict" || CauseChangesRequested != "changes-requested" ||
		CauseThreads != "threads" || CauseEjected != "ejected" || CauseDraftReady != "draft-ready" || CauseNoReviewer != "no-reviewer" {
		t.Error("a persisted cause key changed")
	}
}

func TestBots(t *testing.T) {
	_, r, _ := load(t, "bot_dependabot", nil)
	if !r.Bot || r.BotLabel != "Dependabot" || r.Mine {
		t.Errorf("bot=%v label=%q mine=%v", r.Bot, r.BotLabel, r.Mine)
	}
	_, r, _ = load(t, "bot_dependabot", func(o *Options) {
		o.Bots = []BotPattern{{Pattern: "depend*", Label: "Deps"}}
	})
	if r.BotLabel != "Deps" {
		t.Errorf("configured label not used: %q", r.BotLabel)
	}
	// A human whose login matches a configured pattern counts as a bot.
	_, r, _ = load(t, "review_direct", func(o *Options) {
		o.Bots = []BotPattern{{Pattern: "user-5", Label: "Release robot"}}
	})
	if !r.Bot || r.BotLabel != "Release robot" || r.Bucket != ReviewRequested {
		t.Errorf("bot=%v label=%q bucket=%q", r.Bot, r.BotLabel, r.Bucket)
	}
}

func TestTeamRequestsAreMine(t *testing.T) {
	_, r, _ := load(t, "review_team_only", func(o *Options) { o.TeamRequestsAreMine = true })
	if r.Bucket != ReviewRequested {
		t.Errorf("bucket = %q", r.Bucket)
	}
}

func TestPokeThreshold(t *testing.T) {
	// user-2 has been holding for 2d 1h of business time.
	pr, r, o := load(t, "authored_optional_check_pending", func(o *Options) { o.PokeAfter = 72 * time.Hour })
	if r.Bucket != Poke {
		t.Fatalf("bucket = %q", r.Bucket)
	}
	if got := FormatIdle(r.Holders[0].Idle); got != "2d 1h" {
		t.Errorf("idle = %s", got)
	}
	if p := PokeList([]fetch.PR{pr}, []Result{r}, o); len(p) != 0 {
		t.Errorf("poked below threshold: %+v", p)
	}
}

func TestNoRequiredChecks(t *testing.T) {
	// Strip isRequired everywhere: with the fallback on, a failure still counts.
	strip := func(pr *fetch.PR) {
		ctx := pr.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes
		for i := range ctx {
			ctx[i].IsRequired = false
		}
	}
	f, err := fetch.LoadFixture(filepath.Join("..", "..", "testdata", "authored_required_failed.json"))
	if err != nil {
		t.Fatal(err)
	}
	strip(&f.Node)
	o := Options{Me: f.Viewer, Now: f.FetchedAt, AllChecksWhenNoneRequired: true}
	if r := Classify(f.Node, o); r.Bucket != ActNow {
		t.Errorf("fallback on: bucket = %q", r.Bucket)
	}
	o.AllChecksWhenNoneRequired = false
	if r := Classify(f.Node, o); r.Bucket != Poke {
		t.Errorf("fallback off: bucket = %q", r.Bucket)
	}
}

func TestBusinessDuration(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		from, to string
		want     time.Duration
	}{
		{"2026-11-16T16:00:00Z", "2026-11-18T17:00:00Z", 49 * time.Hour}, // Mon -> Wed
		{"2026-11-13T15:00:00Z", "2026-11-16T15:00:00Z", 24 * time.Hour}, // Fri -> Mon skips the weekend
		{"2026-11-14T10:00:00Z", "2026-11-15T10:00:00Z", 0},              // Sat -> Sun
		{"2026-11-18T17:00:00Z", "2026-11-18T12:00:00Z", 0},              // negative
	}
	for _, c := range cases {
		if got := BusinessDuration(at(c.from), at(c.to)); got != c.want {
			t.Errorf("%s -> %s = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// Local midnight can be skipped by a DST change (Santiago springs forward at
// midnight); the walk must still advance.
func TestBusinessDurationSkippedMidnight(t *testing.T) {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 3, 6, 0, 0, 0, santiago) // Thursday
	to := time.Date(2026, 9, 7, 12, 0, 0, 0, santiago)  // Monday
	done := make(chan time.Duration, 1)
	go func() { done <- BusinessDuration(from, to) }()
	select {
	case got := <-done:
		// Thu 06:00 -> Sat 00:00 is 42h; Mon 00:00 -> 12:00 is 12h.
		if got != 54*time.Hour {
			t.Errorf("BusinessDuration = %v, want 54h", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BusinessDuration did not return: infinite loop")
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"Org-1/archived-*", "org-1/archived-thing", true},
		{"Org-1/archived-*", "Org-1/repo-1", false},
		{"*[bot]", "renovate[bot]", true},
		{"*[bot]", "robot", false},
		{"dependabot*", "dependabot", true},
	} {
		if got := Glob(c.pat, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v", c.pat, c.s, got)
		}
	}
}
