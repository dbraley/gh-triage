// Package snooze hides a PR from the report until something happens. The
// wake rules are pure functions of the PR, its classification and the clock
// passed in; only Store touches the filesystem.
package snooze

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
)

// Entry is one snoozed PR.
type Entry struct {
	URL       string          `json:"url"` // canonical PR URL; the store key (see Key)
	SnoozedAt time.Time       `json:"snoozedAt"`
	Until     time.Time       `json:"until"`            // wall-clock wake time
	HeadOID   string          `json:"headOid"`          // head commit when snoozed
	Bucket    classify.Bucket `json:"bucket"`           // bucket when snoozed
	Causes    []string        `json:"causes,omitempty"` // classify.Result.Causes when snoozed
}

type Options struct {
	Me   string                // viewer login
	Bots []classify.BotPattern // configured bot patterns (DetectBot also applies defaults)
	Now  time.Time
}

// Evaluate reports whether the snooze still holds. When it does not, reason
// says why in a short phrase suitable for a report line. Rules are checked in
// order and the first hit wins.
func Evaluate(e Entry, pr fetch.PR, res classify.Result, o Options) (snoozed bool, reason string) {
	if !o.Now.Before(e.Until) {
		return false, "snooze expired"
	}
	// Either OID may be missing (hand-edited entry, partial data); don't wake on that.
	if oid := pr.Head().OID; oid != "" && e.HeadOID != "" && oid != e.HeadOID {
		return false, "new push"
	}
	if res.Bucket != e.Bucket {
		if e.Bucket == "" { // hand-edited entry
			return false, fmt.Sprintf("now %s", res.Bucket)
		}
		return false, fmt.Sprintf("now %s (was %s)", res.Bucket, e.Bucket)
	}
	// ACT NOW is a union of unrelated problems: a new one wakes the snooze
	// even though the bucket is the same. A problem going away does not.
	if c := newCause(e.Causes, res.Causes); c != "" {
		return false, "new problem: " + causeText(c)
	}
	if who := firstHumanActivity(e.SnoozedAt, pr, o); who != "" {
		return false, who
	}
	if reviewRequestedAgain(e.SnoozedAt, pr, o) {
		return false, "review requested again"
	}
	return true, ""
}

// newCause returns the first of now that was not among then.
func newCause(then, now []string) string {
	for _, c := range now {
		if !slices.Contains(then, c) {
			return c
		}
	}
	return ""
}

// causeText words a classify cause key for a report line. An unknown key is
// shown as is.
func causeText(key string) string {
	switch key {
	case classify.CauseChecksFailed:
		return "a required check failed"
	case classify.CauseConflict:
		return "merge conflict"
	case classify.CauseChangesRequested:
		return "changes requested"
	case classify.CauseThreads:
		return "unresolved threads with new activity"
	case classify.CauseEjected:
		return "removed from the merge queue"
	case classify.CauseDraftReady:
		return "approved but still a draft"
	case classify.CauseNoReviewer:
		return "nobody is reviewing it"
	}
	return key
}

// reviewRequestedAgain reports a review request for the viewer (a person, not
// a team) made after since.
func reviewRequestedAgain(since time.Time, pr fetch.PR, o Options) bool {
	for _, ev := range pr.TimelineItems.Nodes {
		rv := ev.RequestedReviewer
		if ev.Typename == "ReviewRequestedEvent" && ev.CreatedAt.After(since) &&
			rv != nil && !rv.IsTeam() && fetch.EqualFold(rv.Login, o.Me) {
			return true
		}
	}
	return false
}

// firstHumanActivity returns "new comment by x" / "new review by x" for the
// earliest comment or review after since from a human other than the viewer.
func firstHumanActivity(since time.Time, pr fetch.PR, o Options) string {
	var (
		best time.Time
		what string
	)
	consider := func(kind string, a fetch.Actor, at time.Time) {
		if !at.After(since) || (what != "" && !at.Before(best)) {
			return
		}
		if a.Login == "" || fetch.EqualFold(a.Login, o.Me) {
			return
		}
		if bot, _ := classify.DetectBot(a, o.Bots); bot {
			return
		}
		best, what = at, "new "+kind+" by "+a.Login
	}
	for _, c := range pr.Comments.Nodes {
		consider("comment", c.Author, c.CreatedAt)
	}
	for _, r := range pr.Reviews.Nodes {
		consider("review", r.Author, r.SubmittedAt)
	}
	for _, t := range pr.ReviewThreads.Nodes {
		for _, c := range t.Comments.Nodes {
			consider("comment", c.Author, c.CreatedAt)
		}
	}
	return what
}

// Until returns the wall-clock time that is d of business time after from
// (weekends skipped, judged in from's location). Inverse of
// classify.BusinessDuration.
func Until(from time.Time, d time.Duration) time.Time {
	cur := from
	for d > 0 {
		// Calendar midnights, as BusinessDuration walks them, so DST days agree.
		next := time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, cur.Location())
		if !next.After(cur) { // midnight skipped by a DST gap
			next = cur.Add(24 * time.Hour)
		}
		if wd := cur.Weekday(); wd == time.Saturday || wd == time.Sunday {
			cur = next
			continue
		}
		if seg := next.Sub(cur); seg < d {
			d -= seg
			cur = next
		} else {
			return cur.Add(d)
		}
	}
	return cur
}

var forRE = regexp.MustCompile(`^(?:(\d+)d)?(?:(\d+)h)?$`)

// maxFor bounds each component so the Duration arithmetic cannot overflow.
const maxFor = 3650

// ParseFor parses "3d" (business days, 24h each), "12h" or "1d12h".
func ParseFor(s string) (time.Duration, error) {
	m := forRE.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, fmt.Errorf("invalid duration %q: want e.g. 3d, 12h or 1d12h", s)
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n > maxFor {
			return 0, fmt.Errorf("invalid duration %q: too large", s)
		}
		d += time.Duration(n) * unit
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", s)
	}
	return d, nil
}
