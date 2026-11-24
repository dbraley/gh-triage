package classify

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbraley/gh-triage/internal/fetch"
)

func itoa(n int) string { return strconv.Itoa(n) }

// Classify puts one PR in a bucket and records why.
func Classify(pr fetch.PR, o Options) Result {
	c := &classifier{pr: pr, o: o}
	c.r.SortTime = pr.CreatedAt
	c.r.Mine = fetch.EqualFold(pr.Author.Login, o.Me)
	c.r.Bot, c.r.BotLabel = DetectBot(pr.Author, o.Bots)
	c.gather()

	switch {
	case c.r.Mine:
		c.note("authored by you: applying author rules")
		c.authored()
	case c.r.Bot:
		c.note("authored by a bot (%s): applying reviewer rules, then author rules (nobody else owns it)", c.r.BotLabel)
		if c.reviewer() {
			c.r.Holders = c.holders
		} else {
			c.authored()
		}
	default:
		c.note("authored by %s: applying reviewer rules", pr.Author.Login)
		if !c.reviewer() {
			c.nothing()
		}
	}
	c.note("=> %s: %s", c.r.Bucket, c.r.Reason)
	return c.r
}

// PokeList returns everyone holding a PR for at least o.PokeAfter of
// business time, most idle first. results[i] must be Classify(prs[i]).
func PokeList(prs []fetch.PR, results []Result, o Options) []PokeEntry {
	var out []PokeEntry
	for i, r := range results {
		for _, h := range r.Holders {
			if h.Idle >= o.PokeAfter {
				out = append(out, PokeEntry{Holder: h, Ref: prs[i].Ref(), Title: prs[i].Title, URL: prs[i].URL, Bot: r.Bot})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Holder.Idle > out[j].Holder.Idle })
	return out
}

// DetectBot reports whether an author is a bot and what to call it.
// Configured patterns win over defaults; __typename Bot is always a bot.
func DetectBot(a fetch.Actor, configured []BotPattern) (bool, string) {
	for _, p := range append(append([]BotPattern{}, configured...), DefaultBots...) {
		if a.Login != "" && Glob(p.Pattern, a.Login) {
			if p.Label == "" {
				return true, a.Login
			}
			return true, p.Label
		}
	}
	return a.Typename == "Bot", a.Login
}

type classifier struct {
	pr fetch.PR
	o  Options
	r  Result

	head          fetch.Commit
	lastPush      time.Time // head commit date or latest force-push
	waitingSince  time.Time // lastPush, or later: PR opened / marked ready
	ci            ciState
	approved      bool
	conflict      bool
	changesOnHead []string // reviewers whose CHANGES_REQUESTED is on the head commit
	holders       []Holder // set only when review is the one thing missing
}

func (c *classifier) note(format string, a ...any) {
	c.r.Trail = append(c.r.Trail, fmt.Sprintf(format, a...))
}

func (c *classifier) set(b Bucket, format string, a ...any) {
	c.r.Bucket, c.r.Reason = b, fmt.Sprintf(format, a...)
}

func (c *classifier) isMe(login string) bool { return fetch.EqualFold(login, c.o.Me) }

func (c *classifier) isMyTeam(name string) bool {
	for _, t := range c.o.Teams {
		if fetch.EqualFold(t, name) {
			return true
		}
	}
	return false
}

func (c *classifier) isBot(a fetch.Actor) bool {
	b, _ := DetectBot(a, c.o.Bots)
	return b
}

// idle is business time elapsed since t.
func (c *classifier) idle(t time.Time) time.Duration { return BusinessDuration(t, c.o.Now) }

// gather derives the facts every rule shares and notes them in the trail.
func (c *classifier) gather() {
	pr := c.pr
	c.head = pr.Head()
	c.lastPush = c.head.CommittedDate
	c.waitingSince = pr.CreatedAt
	for _, ev := range pr.TimelineItems.Nodes {
		switch ev.Typename {
		case "HeadRefForcePushedEvent":
			c.lastPush = later(c.lastPush, ev.CreatedAt)
		case "ReadyForReviewEvent":
			c.waitingSince = later(c.waitingSince, ev.CreatedAt)
		}
	}
	c.waitingSince = later(c.waitingSince, c.lastPush)
	c.note("head commit %s, last push %s", short(c.head.OID), c.lastPush.Format(time.RFC3339))

	c.ci = evalCI(c.head, c.o.AllChecksWhenNoneRequired)
	c.note("checks on head: %s", c.ci.describe())

	var approvers, changes []string
	for _, rv := range pr.LatestOpinionatedReviews.Nodes {
		switch rv.State {
		case "APPROVED":
			approvers = append(approvers, rv.Author.Login)
		case "CHANGES_REQUESTED":
			changes = append(changes, rv.Author.Login)
			if c.reviewedHead(rv) {
				c.changesOnHead = append(c.changesOnHead, rv.Author.Login)
			}
		}
	}
	// reviewDecision is null when the branch doesn't require reviews; fall
	// back to "someone approved and nobody objected".
	c.approved = pr.ReviewDecision == "APPROVED" ||
		(pr.ReviewDecision == "" && len(approvers) > 0 && len(changes) == 0)
	c.note("reviewDecision=%s approvals=%v changes-requested=%v (on head: %v) => approved=%v",
		orNone(pr.ReviewDecision), approvers, changes, c.changesOnHead, c.approved)

	c.conflict = pr.Mergeable == "CONFLICTING"
	c.note("mergeable=%s mergeStateStatus=%s draft=%v inMergeQueue=%v",
		orNone(pr.Mergeable), orNone(pr.MergeStateStatus), pr.IsDraft, pr.IsInMergeQueue)

	if (c.r.Mine || c.r.Bot) && c.ci.green() && !c.approved && !c.conflict && !pr.IsInMergeQueue && len(c.changesOnHead) == 0 {
		c.holders = c.findHolders()
		c.note("review is the only thing missing; holding the ball: %s", describeHolders(c.holders))
	}
}

// reviewedHead reports whether a review covers the current head commit.
func (c *classifier) reviewedHead(rv fetch.Review) bool {
	if oid := rv.CommitOID(); oid != "" {
		return oid == c.head.OID
	}
	return !rv.SubmittedAt.Before(c.lastPush) // commit gone (force-push): go by time
}

// findHolders lists who a review is waiting on: requested reviewers who
// haven't responded, plus reviewers whose last word was on an older commit.
// Bots, the author, and (on PRs that aren't mine) me and my teams are never
// holders.
func (c *classifier) findHolders() []Holder {
	requestedAt := map[string]time.Time{}
	for _, ev := range c.pr.TimelineItems.Nodes {
		if ev.Typename == "ReviewRequestedEvent" && ev.RequestedReviewer != nil {
			k := strings.ToLower(ev.RequestedReviewer.Name())
			requestedAt[k] = later(requestedAt[k], ev.CreatedAt)
		}
	}
	var hs []Holder
	seen := map[string]bool{}
	add := func(who string, team, rereview bool, since time.Time) {
		k := strings.ToLower(who)
		if who == "" || seen[k] || fetch.EqualFold(who, c.pr.Author.Login) {
			return
		}
		if !c.r.Mine && (c.isMe(who) || c.isMyTeam(who)) {
			return
		}
		seen[k] = true
		hs = append(hs, Holder{Who: who, IsTeam: team, ReReview: rereview, Since: since, Idle: c.idle(since)})
	}
	for _, n := range c.pr.ReviewRequests.Nodes {
		rv := n.RequestedReviewer
		if !rv.IsTeam() && c.isBot(fetch.Actor{Typename: rv.Typename, Login: rv.Login}) {
			continue
		}
		add(rv.Name(), rv.IsTeam(), false, later(c.waitingSince, requestedAt[strings.ToLower(rv.Name())]))
	}
	for _, rv := range c.pr.LatestReviews.Nodes {
		if c.isBot(rv.Author) || rv.State == "APPROVED" || rv.State == "PENDING" || c.reviewedHead(rv) {
			continue
		}
		add(rv.Author.Login, false, true, c.waitingSince)
	}
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Since.Before(hs[j].Since) })
	return hs
}

// authored applies the author rules; first match wins.
func (c *classifier) authored() {
	pr := c.pr

	// 1. ACT NOW: anything only the author can fix.
	var act []string
	if len(c.ci.failed) > 0 {
		act = append(act, "required check failed: "+strings.Join(c.ci.failed, ", "))
	}
	if c.conflict {
		act = append(act, "merge conflict with the base branch")
	}
	if len(c.changesOnHead) > 0 {
		act = append(act, "changes requested by "+strings.Join(c.changesOnHead, ", ")+" on the current commit")
	}
	if n, who := c.threadsNeedingAuthor(); n > 0 {
		act = append(act, fmt.Sprintf("%d unresolved thread%s with activity since your last push (%s)", n, plural(n), strings.Join(who, ", ")))
	}
	if why := c.kickedFromQueue(); why != "" {
		act = append(act, why)
	}
	if len(act) > 0 {
		c.set(ActNow, "%s", strings.Join(act, "; "))
		return
	}
	c.note("1 ACT NOW: no - no failed required check, conflict, changes requested on head, new thread activity, or queue ejection")

	// 2. IN MERGE QUEUE.
	if pr.IsInMergeQueue {
		c.set(InMergeQueue, "%s", describeQueue(pr.MergeQueueEntry))
		return
	}
	c.note("2 IN MERGE QUEUE: no")

	green := c.ci.green()

	// 3. READY TO ENQUEUE.
	if c.approved && green {
		if pr.IsDraft {
			c.set(ActNow, "approved and green but still a draft: mark it ready for review")
			return
		}
		reason := "approved, required checks green, no conflicts: add to the merge queue"
		if !pr.IsMergeQueueEnabled {
			reason = "approved, required checks green, no conflicts: merge it (this repo has no merge queue)"
		}
		if pr.AutoMergeRequest != nil {
			reason += " (auto-merge is on but it has not been queued)"
		}
		if pr.MergeStateStatus == "BLOCKED" {
			reason += " (GitHub still reports BLOCKED: a required check may not have reported yet)"
		}
		c.set(ReadyToEnqueue, "%s", reason)
		return
	}
	c.note("3 READY TO ENQUEUE: no (approved=%v, required checks green=%v)", c.approved, green)

	// 4. POKE: review is the only thing missing.
	if green && !c.approved {
		if len(c.holders) > 0 {
			c.r.Holders = c.holders
			c.r.SortTime = c.holders[0].Since
			c.set(Poke, "required checks green, waiting on %s", describeHolders(c.holders))
			return
		}
		c.note("4 POKE: no - checks are green and it is not approved, but nobody is holding it")
		if pr.IsDraft {
			c.set(Draft, "draft; required checks green, no reviewer requested")
			return
		}
		// Not "waiting on reviewers": there are none to wait on.
		c.set(ActNow, "required checks green but nobody is holding it: request a review%s", c.lastWord())
		return
	}
	c.note("4 POKE: no (required checks not green)")

	// 5. WAITING ON CI: only a required check on the head commit counts.
	if len(c.ci.pending) > 0 {
		c.set(WaitingOnCI, "required check in progress: %s", strings.Join(c.ci.pending, ", "))
		return
	}
	c.note("5 WAITING ON CI: no")

	// 6. DRAFT, or nothing matched (should not happen for an open PR).
	if pr.IsDraft {
		c.set(Draft, "draft")
		return
	}
	c.set(Nothing, "no rule matched; run with --dump and add a fixture")
}

// lastWord explains a holder-less PR when a reviewer already weighed in on
// the current commit without approving.
func (c *classifier) lastWord() string {
	for _, rv := range c.pr.LatestReviews.Nodes {
		if rv.State == "COMMENTED" && !c.isBot(rv.Author) && !fetch.EqualFold(rv.Author.Login, c.pr.Author.Login) {
			return fmt.Sprintf(" (%s commented on the current commit: respond or re-request)", rv.Author.Login)
		}
	}
	return ""
}

// threadsNeedingAuthor counts unresolved threads whose latest comment is
// from someone other than the author and newer than the last push.
func (c *classifier) threadsNeedingAuthor() (int, []string) {
	n := 0
	var who []string
	for _, t := range c.pr.ReviewThreads.Nodes {
		cs := t.Comments.Nodes
		if t.IsResolved || len(cs) == 0 {
			continue
		}
		last := cs[len(cs)-1]
		if fetch.EqualFold(last.Author.Login, c.pr.Author.Login) || !last.CreatedAt.After(c.lastPush) {
			continue
		}
		n++
		who = appendUnique(who, last.Author.Login)
	}
	return n, who
}

// kickedFromQueue returns a reason if the PR was ejected from the merge
// queue (or is stuck in it unmergeable) and nothing has been pushed since.
func (c *classifier) kickedFromQueue() string {
	if e := c.pr.MergeQueueEntry; c.pr.IsInMergeQueue && e != nil && e.State == "UNMERGEABLE" {
		return "merge queue entry is unmergeable"
	}
	if c.pr.IsInMergeQueue {
		return ""
	}
	var added, removed *fetch.TimelineItem
	for i, ev := range c.pr.TimelineItems.Nodes {
		switch ev.Typename {
		case "AddedToMergeQueueEvent":
			added = &c.pr.TimelineItems.Nodes[i]
		case "RemovedFromMergeQueueEvent":
			removed = &c.pr.TimelineItems.Nodes[i]
		}
	}
	if removed == nil || (added != nil && added.CreatedAt.After(removed.CreatedAt)) {
		return ""
	}
	if !removed.CreatedAt.After(c.lastPush) {
		c.note("removed from the merge queue %s, but you pushed since", removed.CreatedAt.Format(time.RFC3339))
		return ""
	}
	if a := removed.Actor; a != nil && (c.isMe(a.Login) || fetch.EqualFold(a.Login, c.pr.Author.Login)) {
		c.note("removed from the merge queue by %s: not an ejection", a.Login)
		return ""
	}
	why := "removed from the merge queue " + ago(c.o.Now.Sub(removed.CreatedAt)) + " ago"
	if removed.Reason != "" {
		why += " (" + removed.Reason + ")"
	}
	return why
}

// reviewer applies the rules for PRs by others; false means none matched.
func (c *classifier) reviewer() bool {
	pr := c.pr
	direct := false
	var teams []string
	for _, n := range pr.ReviewRequests.Nodes {
		rv := n.RequestedReviewer
		switch {
		case rv.IsTeam() && c.isMyTeam(rv.Name()):
			teams = append(teams, rv.Name())
		case !rv.IsTeam() && c.isMe(rv.Login):
			direct = true
		}
	}
	// When the request was made; if the event is missing, when the PR
	// became reviewable.
	requested := func(names ...string) time.Time {
		var t time.Time
		for _, ev := range pr.TimelineItems.Nodes {
			if ev.Typename != "ReviewRequestedEvent" || ev.RequestedReviewer == nil {
				continue
			}
			for _, n := range names {
				if fetch.EqualFold(ev.RequestedReviewer.Name(), n) {
					t = later(t, ev.CreatedAt)
				}
			}
		}
		if t.IsZero() {
			return c.waitingSince
		}
		return t
	}

	// 1. REVIEW REQUESTED of me directly.
	if direct {
		c.r.SortTime = requested(c.o.Me)
		c.set(ReviewRequested, "your review was requested %s ago", FormatIdle(c.idle(c.r.SortTime)))
		return true
	}
	if len(teams) > 0 && c.o.TeamRequestsAreMine {
		c.r.SortTime = requested(teams...)
		c.set(ReviewRequested, "review requested %s ago via your team %s", FormatIdle(c.idle(c.r.SortTime)), strings.Join(teams, ", "))
		return true
	}
	c.note("1 REVIEW REQUESTED: no (not requested directly)")

	// 2. RE-REVIEW: my last review was on an older commit and no longer settles it.
	var mine *fetch.Review
	for i, rv := range pr.LatestReviews.Nodes {
		if c.isMe(rv.Author.Login) {
			mine = &pr.LatestReviews.Nodes[i]
		}
	}
	switch {
	case mine == nil:
		c.note("2 RE-REVIEW: no (you have not reviewed)")
	case mine.State == "APPROVED":
		c.note("2 RE-REVIEW: no (your approval still stands)")
	case c.reviewedHead(*mine):
		c.note("2 RE-REVIEW: no (your %s review is on the current commit)", mine.State)
	default:
		c.r.SortTime = c.lastPush
		c.set(ReReview, "you reviewed %s (%s); %s pushed since, %s ago",
			short(mine.CommitOID()), strings.ToLower(strings.ReplaceAll(mine.State, "_", " ")),
			orNone(pr.Author.Login), FormatIdle(c.idle(c.lastPush)))
		return true
	}

	// 3. REPLY OWED: someone answered in an open thread I commented on.
	n := 0
	var who []string
	var oldest time.Time
	for _, t := range pr.ReviewThreads.Nodes {
		cs := t.Comments.Nodes
		if t.IsResolved || len(cs) == 0 || c.isMe(cs[len(cs)-1].Author.Login) {
			continue
		}
		last := cs[len(cs)-1]
		for _, cm := range cs {
			if c.isMe(cm.Author.Login) {
				n++
				who = appendUnique(who, last.Author.Login)
				if oldest.IsZero() || last.CreatedAt.Before(oldest) {
					oldest = last.CreatedAt
				}
				break
			}
		}
	}
	if n > 0 {
		c.r.SortTime = oldest
		c.set(ReplyOwed, "%s replied in %d open thread%s you commented on", strings.Join(who, ", "), n, plural(n))
		return true
	}
	c.note("3 REPLY OWED: no")

	// 4. TEAM-ONLY.
	if len(teams) > 0 {
		c.r.SortTime = requested(teams...)
		c.set(TeamOnly, "requested of your team %s, not you directly (%s ago)", strings.Join(teams, ", "), FormatIdle(c.idle(c.r.SortTime)))
		return true
	}
	c.note("4 TEAM REVIEW: no")
	return false
}

func (c *classifier) nothing() {
	for _, rv := range c.pr.LatestReviews.Nodes {
		if c.isMe(rv.Author.Login) {
			c.set(Nothing, "you already reviewed (%s); waiting on the author or other reviewers", strings.ToLower(rv.State))
			return
		}
	}
	c.set(Nothing, "nothing is requested of you")
}

// ciState summarises the checks on the head commit. Only required checks
// can fail or hold a PR; skipped and neutral count as passed.
type ciState struct {
	failed, pending       []string // required
	optFailed, optPending []string // noted in the trail, never decisive
	total, required       int
	fallback              bool // nothing required, so everything was treated as required
	truncated             bool
}

func (s ciState) green() bool { return len(s.failed) == 0 && len(s.pending) == 0 }

func (s ciState) describe() string {
	out := fmt.Sprintf("%d total, %d required", s.total, s.required)
	if s.fallback {
		out += " (none marked required: treating all as required)"
	}
	for _, p := range []struct {
		label string
		names []string
	}{{"required failed", s.failed}, {"required pending", s.pending}, {"optional failed (ignored)", s.optFailed}, {"optional pending (ignored)", s.optPending}} {
		if len(p.names) > 0 {
			out += fmt.Sprintf("; %s: %s", p.label, strings.Join(p.names, ", "))
		}
	}
	if s.truncated {
		out += "; WARNING: more than 100 checks, only the first 100 were read"
	}
	return out
}

func evalCI(head fetch.Commit, allWhenNoneRequired bool) ciState {
	var s ciState
	if head.StatusCheckRollup == nil {
		return s
	}
	ctx := head.StatusCheckRollup.Contexts
	s.truncated = ctx.TotalCount > len(ctx.Nodes)

	// A rerun shows up as a second entry with the same name: keep the newest.
	latest := map[string]fetch.Check{}
	var names []string
	for _, ch := range ctx.Nodes {
		name := ch.DisplayName()
		prev, ok := latest[name]
		if !ok {
			names = append(names, name)
		}
		if !ok || !ch.When().Before(prev.When()) {
			ch.IsRequired = ch.IsRequired || prev.IsRequired
			latest[name] = ch
		}
	}
	sort.Strings(names)
	s.total = len(names)
	for _, n := range names {
		if latest[n].IsRequired {
			s.required++
		}
	}
	s.fallback = s.required == 0 && s.total > 0 && allWhenNoneRequired

	for _, n := range names {
		ch := latest[n]
		required := ch.IsRequired || s.fallback
		switch outcome(ch) {
		case "fail":
			if required {
				s.failed = append(s.failed, n)
			} else {
				s.optFailed = append(s.optFailed, n)
			}
		case "pending":
			if required {
				s.pending = append(s.pending, n)
			} else {
				s.optPending = append(s.optPending, n)
			}
		}
	}
	return s
}

func outcome(ch fetch.Check) string {
	if ch.Typename == "StatusContext" {
		switch ch.State {
		case "SUCCESS":
			return "pass"
		case "PENDING", "EXPECTED":
			return "pending"
		}
		return "fail"
	}
	if ch.Status != "COMPLETED" {
		return "pending" // QUEUED, IN_PROGRESS, WAITING, PENDING, REQUESTED
	}
	switch ch.Conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED", "STALE":
		return "pass"
	}
	return "fail" // FAILURE, TIMED_OUT, CANCELLED, ACTION_REQUIRED, STARTUP_FAILURE
}

func describeHolders(hs []Holder) string {
	if len(hs) == 0 {
		return "nobody"
	}
	var parts []string
	for _, h := range hs {
		kind := "review"
		if h.ReReview {
			kind = "re-review"
		}
		parts = append(parts, fmt.Sprintf("%s from %s (%s)", kind, h.Who, FormatIdle(h.Idle)))
	}
	return strings.Join(parts, ", ")
}

func describeQueue(e *fetch.MergeQueueEntry) string {
	if e == nil {
		return "in the merge queue"
	}
	s := fmt.Sprintf("in the merge queue: position %d, %s", e.Position, strings.ToLower(strings.ReplaceAll(e.State, "_", " ")))
	if e.EstimatedTimeToMerge != nil {
		s += ", ETA ~" + ago(time.Duration(*e.EstimatedTimeToMerge)*time.Second)
	}
	return s
}

// ago renders a wall-clock duration coarsely: "40m", "6h", "3d".
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + "h"
	}
	return itoa(int(d.Hours()/24)) + "d"
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func short(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return orNone(oid)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
