package snooze

import (
	"testing"
	"time"
	_ "time/tzdata" // zone rows must run on machines without system tzdata

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
)

var (
	t0      = time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC) // a Monday
	snoozed = t0
	now     = t0.Add(time.Hour)
)

func baseEntry() Entry {
	return Entry{
		URL:       "https://github.com/org-1/repo-1/pull/1",
		SnoozedAt: snoozed,
		Until:     snoozed.Add(72 * time.Hour),
		HeadOID:   "aaa",
		Bucket:    classify.Poke,
	}
}

func prWith(oid string) fetch.PR {
	var pr fetch.PR
	pr.Commits.Nodes = make([]struct {
		Commit fetch.Commit `json:"commit"`
	}, 1)
	pr.Commits.Nodes[0].Commit.OID = oid
	return pr
}

func comment(login, typename string, at time.Time) fetch.Comment {
	return fetch.Comment{Author: fetch.Actor{Login: login, Typename: typename}, CreatedAt: at}
}

func reviewRequest(login string, team bool, at time.Time) fetch.TimelineItem {
	rv := &fetch.Reviewer{Typename: "User", Login: login}
	if team {
		rv = &fetch.Reviewer{Typename: "Team"}
	}
	return fetch.TimelineItem{Typename: "ReviewRequestedEvent", CreatedAt: at, RequestedReviewer: rv}
}

func TestEvaluate(t *testing.T) {
	after := snoozed.Add(30 * time.Minute)
	tests := []struct {
		name   string
		mutate func(*Entry, *fetch.PR, *classify.Result, *Options)
		want   bool
		reason string
	}{
		{"still snoozed", func(*Entry, *fetch.PR, *classify.Result, *Options) {}, true, ""},
		{"expired", func(_ *Entry, _ *fetch.PR, _ *classify.Result, o *Options) { o.Now = snoozed.Add(72 * time.Hour) }, false, "snooze expired"},
		{"new push", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) { *pr = prWith("bbb") }, false, "new push"},
		{"missing head oid does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) { *pr = prWith("") }, true, ""},
		{"bucket changed", func(_ *Entry, _ *fetch.PR, r *classify.Result, _ *Options) { r.Bucket = classify.ReadyToEnqueue }, false, "now READY TO ENQUEUE (was POKE)"},
		{"new comment", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", after)}
		}, false, "new comment by user-3"},
		{"my comment does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("ME", "User", after)}
		}, true, ""},
		{"bot login does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("github-actions", "", after)}
		}, true, ""},
		{"Bot typename does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("helper-1", "Bot", after)}
		}, true, ""},
		{"empty login does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("", "", after)}
		}, true, ""},
		{"comment before snooze does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", snoozed.Add(-time.Minute))}
		}, true, ""},
		{"comment exactly at snooze time does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", snoozed)}
		}, true, ""},
		{"review by another human wakes", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Reviews.Nodes = []fetch.Review{{Author: fetch.Actor{Login: "user-4"}, SubmittedAt: after}}
		}, false, "new review by user-4"},
		{"configured bot pattern does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, o *Options) {
			o.Bots = []classify.BotPattern{{Pattern: "release-bot-*", Label: "Release bot"}}
			pr.Comments.Nodes = []fetch.Comment{comment("release-bot-1", "User", after)}
		}, true, ""},
		{"same login without the bot pattern wakes", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("release-bot-1", "User", after)}
		}, false, "new comment by release-bot-1"},
		{"empty bucket in a hand-edited entry", func(e *Entry, _ *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket = ""
			r.Bucket = classify.ActNow
		}, false, "now ACT NOW"},
		{"new cause in the same bucket wakes", func(e *Entry, _ *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket, r.Bucket = classify.ActNow, classify.ActNow
			e.Causes = []string{classify.CauseChecksFailed}
			r.Causes = []string{classify.CauseChecksFailed, classify.CauseConflict}
		}, false, "new problem: merge conflict"},
		{"same causes stay snoozed", func(e *Entry, _ *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket, r.Bucket = classify.ActNow, classify.ActNow
			e.Causes = []string{classify.CauseChecksFailed, classify.CauseConflict}
			r.Causes = []string{classify.CauseConflict, classify.CauseChecksFailed}
		}, true, ""},
		{"a cause going away does not wake", func(e *Entry, _ *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket, r.Bucket = classify.ActNow, classify.ActNow
			e.Causes = []string{classify.CauseChecksFailed, classify.CauseConflict}
			r.Causes = []string{classify.CauseConflict}
		}, true, ""},
		{"unknown cause key is shown as is", func(e *Entry, _ *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket, r.Bucket = classify.ActNow, classify.ActNow
			r.Causes = []string{"future-cause"}
		}, false, "new problem: future-cause"},
		{"new cause beats comment", func(e *Entry, pr *fetch.PR, r *classify.Result, _ *Options) {
			e.Bucket, r.Bucket = classify.ActNow, classify.ActNow
			r.Causes = []string{classify.CauseThreads}
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", after)}
		}, false, "new problem: unresolved threads with new activity"},
		{"review requested of me again wakes", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{reviewRequest("Me", false, after)}
		}, false, "review requested again"},
		{"review requested before the snooze does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{reviewRequest("me", false, snoozed.Add(-time.Minute))}
		}, true, ""},
		{"review requested exactly at snooze time does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{reviewRequest("me", false, snoozed)}
		}, true, ""},
		{"review requested of someone else does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{reviewRequest("user-3", false, after)}
		}, true, ""},
		{"team request does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			ev := reviewRequest("", true, after)
			ev.RequestedReviewer.Slug = "me" // a team named like the viewer
			pr.TimelineItems.Nodes = []fetch.TimelineItem{ev}
		}, true, ""},
		{"other timeline events do not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			ev := reviewRequest("me", false, after)
			ev.Typename = "ReadyForReviewEvent"
			pr.TimelineItems.Nodes = []fetch.TimelineItem{ev}
		}, true, ""},
		{"review request without a reviewer does not wake", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{{Typename: "ReviewRequestedEvent", CreatedAt: after}}
		}, true, ""},
		{"comment beats review request", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.TimelineItems.Nodes = []fetch.TimelineItem{reviewRequest("me", false, after)}
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", after.Add(time.Hour))}
		}, false, "new comment by user-3"},
		{"thread comment wakes", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			var th fetch.Thread
			th.Comments.Nodes = []fetch.Comment{comment("user-5", "User", after)}
			pr.ReviewThreads.Nodes = []fetch.Thread{th}
		}, false, "new comment by user-5"},
		{"earliest qualifying activity is reported", func(_ *Entry, pr *fetch.PR, _ *classify.Result, _ *Options) {
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", after.Add(time.Hour))}
			pr.Reviews.Nodes = []fetch.Review{{Author: fetch.Actor{Login: "user-4"}, SubmittedAt: after}}
		}, false, "new review by user-4"},
		{"expired beats push", func(_ *Entry, pr *fetch.PR, _ *classify.Result, o *Options) {
			o.Now = snoozed.Add(100 * time.Hour)
			*pr = prWith("bbb")
		}, false, "snooze expired"},
		{"push beats bucket", func(_ *Entry, pr *fetch.PR, r *classify.Result, _ *Options) {
			*pr = prWith("bbb")
			r.Bucket = classify.ActNow
		}, false, "new push"},
		{"bucket beats comment", func(_ *Entry, pr *fetch.PR, r *classify.Result, _ *Options) {
			r.Bucket = classify.ActNow
			pr.Comments.Nodes = []fetch.Comment{comment("user-3", "User", after)}
		}, false, "now ACT NOW (was POKE)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, pr, res, o := baseEntry(), prWith("aaa"), classify.Result{Bucket: classify.Poke}, Options{Me: "me", Now: now}
			tt.mutate(&e, &pr, &res, &o)
			got, reason := Evaluate(e, pr, res, o)
			if got != tt.want || reason != tt.reason {
				t.Errorf("Evaluate = (%v, %q), want (%v, %q)", got, reason, tt.want, tt.reason)
			}
		})
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestUntil(t *testing.T) {
	ny, auckland := mustLoc(t, "America/New_York"), mustLoc(t, "Pacific/Auckland")
	fri := time.Date(2025, 1, 10, 15, 0, 0, 0, time.UTC)
	sat := time.Date(2025, 1, 11, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		from time.Time
		d    time.Duration
		want time.Time
	}{
		{"over a weekend", fri, 24 * time.Hour, time.Date(2025, 1, 13, 15, 0, 0, 0, time.UTC)},
		{"within a weekday", t0, 3 * time.Hour, t0.Add(3 * time.Hour)},
		{"starting on a Saturday", sat, 24 * time.Hour, time.Date(2025, 1, 14, 0, 0, 0, 0, time.UTC)},
		{"zero duration", fri, 0, fri},
		{"zero duration on a Saturday", sat, 0, sat},
		// Weekends are judged in from's zone, not UTC.
		{"Friday evening in New York", time.Date(2025, 1, 10, 22, 0, 0, 0, ny), 3 * time.Hour, time.Date(2025, 1, 13, 1, 0, 0, 0, ny)},
		{"Monday just after midnight in Auckland", time.Date(2025, 1, 13, 0, 30, 0, 0, auckland), time.Hour, time.Date(2025, 1, 13, 1, 30, 0, 0, auckland)},
		{"across the spring DST change", time.Date(2025, 3, 7, 12, 0, 0, 0, ny), 24 * time.Hour, time.Date(2025, 3, 10, 12, 0, 0, 0, ny)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Until(tt.from, tt.d); !got.Equal(tt.want) {
				t.Errorf("Until = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUntilInvertsBusinessDuration(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	froms := []time.Time{
		t0,
		time.Date(2025, 1, 10, 15, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 11, 10, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 12, 23, 59, 0, 0, time.UTC),
		time.Date(2025, 3, 7, 12, 0, 0, 0, ny), // spans the US DST change
		time.Date(2025, 11, 1, 8, 0, 0, 0, ny), // Saturday before fall-back
	}
	ds := []time.Duration{time.Hour, 12 * time.Hour, 24 * time.Hour, 36 * time.Hour, 72 * time.Hour, 5 * 24 * time.Hour, 10 * 24 * time.Hour}
	for _, from := range froms {
		for _, d := range ds {
			to := Until(from, d)
			if got := classify.BusinessDuration(from, to); got != d {
				t.Errorf("BusinessDuration(%v, Until(%v, %v)=%v) = %v, want %v", from, from, d, to, got, d)
			}
		}
	}
}

func TestParseFor(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"3d", 72 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"1d12h", 36 * time.Hour, false},
		{"0d6h", 6 * time.Hour, false},
		{"", 0, true},
		{"0d", 0, true},
		{"0h", 0, true},
		{"-1d", 0, true},
		{"abc", 0, true},
		{"3", 0, true},
		{"3w", 0, true},
		{"12h1d", 0, true},
		{"1.5d", 0, true},
		{" 3d", 0, true},
		{"99999999999999999999d", 0, true},
		{"100000d", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseFor(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseFor(%q) = (%v, %v), want (%v, err=%v)", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// returnsWithin fails the test if f does not return in time: the DST-gap bugs
// were infinite loops.
func returnsWithin(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return: infinite loop")
	}
}

// America/Santiago springs forward at midnight, so Sunday 00:00 does not
// exist on the first Saturday night of September.
func TestUntilSkippedMidnight(t *testing.T) {
	santiago := mustLoc(t, "America/Santiago")
	from := time.Date(2026, 9, 3, 6, 0, 0, 0, santiago) // Thursday
	var got time.Time
	returnsWithin(t, func() { got = Until(from, 72*time.Hour) })
	if !got.After(from) {
		t.Fatalf("Until = %v, not after %v", got, from)
	}
	if bd := classify.BusinessDuration(from, got); bd != 72*time.Hour {
		t.Errorf("BusinessDuration(from, Until) = %v, want 72h (until %v)", bd, got)
	}
}
