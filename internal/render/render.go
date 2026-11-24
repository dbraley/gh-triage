// Package render prints classified PRs for the terminal and for Obsidian.
package render

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
)

type Item struct {
	PR  fetch.PR
	Res classify.Result
	// SnoozedUntil is set when a snooze still holds; the item is then listed
	// only in the Snoozed section. Woke is the note for an item whose snooze
	// just ended. Both come from the caller: render never reads the store.
	SnoozedUntil time.Time
	Woke         string
}

func (it Item) snoozed() bool { return !it.SnoozedUntil.IsZero() }

// WakeSummary is what ends a snooze before its time is up, worded once for
// every place that says it.
const WakeSummary = "a push, a comment or review by someone else, or a change in what it needs"

// TimeFormat is how wake times are printed, with the zone abbreviation.
const TimeFormat = "2006-01-02 15:04 MST"

func (it Item) wakesAt() string {
	return it.SnoozedUntil.Format(TimeFormat) + " (or on " + WakeSummary + ")"
}

type Options struct {
	ShowBots     bool
	ShowSnoozed  bool
	Color        bool
	Now          time.Time
	StaleAfter   time.Duration     // business time; 0 disables the marker
	SlackHandles map[string]string // lowercased GitHub login or org/team -> Slack handle
}

// staleBuckets are the ones where someone is waiting on a person.
var staleBuckets = map[classify.Bucket]bool{
	classify.ReviewRequested: true, classify.ReReview: true, classify.ReplyOwed: true,
	classify.TeamOnly: true, classify.Poke: true,
}

type view struct {
	mine, review, bots map[classify.Bucket][]Item
	nMine, nReview     int
	nBots, nNothing    int
	snoozed            []Item // by wake time, earliest first
}

func group(items []Item) view {
	v := view{mine: map[classify.Bucket][]Item{}, review: map[classify.Bucket][]Item{}, bots: map[classify.Bucket][]Item{}}
	sorted := append([]Item{}, items...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Res.SortTime.Before(sorted[j].Res.SortTime) })
	for _, it := range sorted {
		b := it.Res.Bucket
		switch {
		case it.snoozed(): // before the bot case: a snoozed bot PR counts as snoozed
			v.snoozed = append(v.snoozed, it)
		case it.Res.Bot:
			v.bots[b] = append(v.bots[b], it)
			v.nBots++
		case b == classify.Nothing:
			v.nNothing++
		case it.Res.Mine:
			v.mine[b] = append(v.mine[b], it)
			v.nMine++
		default:
			v.review[b] = append(v.review[b], it)
			v.nReview++
		}
	}
	sort.SliceStable(v.snoozed, func(i, j int) bool { return v.snoozed[i].SnoozedUntil.Before(v.snoozed[j].SnoozedUntil) })
	return v
}

// snoozedSummary counts the snoozed items by the bucket they would be in.
func (v view) snoozedSummary() string {
	by := map[classify.Bucket][]Item{}
	for _, it := range v.snoozed {
		by[it.Res.Bucket] = append(by[it.Res.Bucket], it)
	}
	return summary(by)
}

var allBuckets = append(append(append([]classify.Bucket{}, classify.AuthoredOrder...), classify.ReviewOrder...), classify.Nothing)

func (o Options) stale(it Item) bool {
	return o.StaleAfter > 0 && staleBuckets[it.Res.Bucket] &&
		classify.BusinessDuration(it.Res.SortTime, o.Now) >= o.StaleAfter
}

// Nudge is a ready-to-paste Slack message for one poke-list entry.
func Nudge(p classify.PokeEntry, o Options) string {
	who := p.Holder.Who
	if h, ok := o.SlackHandles[strings.ToLower(who)]; ok {
		who = h
	}
	who = "@" + strings.TrimPrefix(who, "@")
	idle := classify.FormatIdle(p.Holder.Idle)
	switch {
	case p.Holder.ReReview:
		return fmt.Sprintf("%s I pushed changes since your last review of %s (%q). Could you take another look? It's been waiting %s and CI is green.", who, p.URL, p.Title, idle)
	case p.Bot:
		return fmt.Sprintf("%s the automated PR %s (%q) has been waiting on your review for %s and CI is green. Could you review or reassign it?", who, p.URL, p.Title, idle)
	}
	return fmt.Sprintf("%s could you review %s (%q)? It's been waiting on you for %s and CI is green.", who, p.URL, p.Title, idle)
}

// ---- terminal ----

const (
	bold   = "1"
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
)

var bucketColor = map[classify.Bucket]string{
	classify.ActNow: red, classify.ReadyToEnqueue: green, classify.Poke: yellow,
	classify.ReviewRequested: red, classify.ReReview: yellow, classify.ReplyOwed: yellow,
}

type term struct {
	w     io.Writer
	color bool
}

func (t term) c(code, s string) string {
	if !t.color || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (t term) section(title string) { fmt.Fprintf(t.w, "\n%s\n", t.c(bold, title)) }

func (t term) buckets(order []classify.Bucket, by map[classify.Bucket][]Item, o Options) {
	for _, b := range order {
		items := by[b]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(t.w, "  %s\n", t.c(bucketColor[b], fmt.Sprintf("%s (%d)", b, len(items))))
		for _, it := range items {
			label := ""
			if it.Res.Bot {
				label = t.c(cyan, "["+it.Res.BotLabel+"] ")
			}
			stale := ""
			if o.stale(it) {
				stale = t.c(red, "  STALE")
			}
			woke := ""
			if it.Woke != "" {
				woke = t.c(cyan, "  ("+it.Woke+")")
			}
			fmt.Fprintf(t.w, "    %s%s  %s%s%s\n", label, t.c(bold, it.PR.Ref()), it.PR.Title, stale, woke)
			fmt.Fprintf(t.w, "        %s\n", it.Res.Reason)
			fmt.Fprintf(t.w, "        %s\n", t.c(dim, it.PR.URL))
		}
	}
}

// Terminal prints the grouped report.
func Terminal(w io.Writer, items []Item, pokes []classify.PokeEntry, o Options) {
	t := term{w, o.Color}
	v := group(items)

	t.section(fmt.Sprintf("MY PULL REQUESTS (%d)", v.nMine))
	t.buckets(classify.AuthoredOrder, v.mine, o)
	t.section(fmt.Sprintf("TO REVIEW (%d)", v.nReview))
	t.buckets(classify.ReviewOrder, v.review, o)

	t.section(fmt.Sprintf("POKE LIST (%d)", len(pokes)))
	for _, p := range pokes {
		kind := "review"
		if p.Holder.ReReview {
			kind = "re-review"
		}
		fmt.Fprintf(w, "    %s  %s on %s, idle %s\n", t.c(bold, p.Holder.Who), kind, p.Ref, classify.FormatIdle(p.Holder.Idle))
		fmt.Fprintf(w, "        %s\n", t.c(dim, Nudge(p, o)))
	}

	if v.nBots > 0 {
		t.section(fmt.Sprintf("AUTOMATED (%d)", v.nBots))
		if o.ShowBots {
			t.buckets(allBuckets, v.bots, o)
		} else {
			fmt.Fprintf(w, "  %s  %s\n", summary(v.bots), t.c(dim, "(--show-bots to expand)"))
		}
	}
	if len(v.snoozed) > 0 {
		t.section(fmt.Sprintf("SNOOZED (%d)", len(v.snoozed)))
		if o.ShowSnoozed {
			for _, it := range v.snoozed {
				fmt.Fprintf(w, "    %s  %s\n", t.c(bold, it.PR.Ref()), it.PR.Title)
				fmt.Fprintf(w, "        %s, wakes %s\n", it.Res.Bucket, it.wakesAt())
				fmt.Fprintf(w, "        %s\n", t.c(dim, it.PR.URL))
			}
		} else {
			fmt.Fprintf(w, "  %s  %s\n", v.snoozedSummary(), t.c(dim, "(--show-snoozed to expand)"))
		}
	}
	if v.nNothing > 0 {
		fmt.Fprintf(w, "\n%s\n", t.c(dim, fmt.Sprintf("%d other open PR(s) you are involved in need nothing from you.", v.nNothing)))
	}
}

func summary(by map[classify.Bucket][]Item) string {
	var parts []string
	for _, b := range allBuckets {
		if n := len(by[b]); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(b))))
		}
	}
	return strings.Join(parts, ", ")
}

// Explain prints the decision trail for one PR.
func Explain(w io.Writer, it Item) {
	fmt.Fprintf(w, "%s  %s\n%s\n\n", it.PR.Ref(), it.PR.Title, it.PR.URL)
	for _, line := range it.Res.Trail {
		fmt.Fprintf(w, "  - %s\n", line)
	}
	fmt.Fprintf(w, "\n%s: %s\n", it.Res.Bucket, it.Res.Reason)
	switch {
	case it.snoozed():
		fmt.Fprintf(w, "\nSnoozed until %s\n", it.wakesAt())
	case it.Woke != "":
		fmt.Fprintf(w, "\nSnooze would wake now: %s\n", it.Woke)
	}
}

// ---- markdown (Obsidian Tasks) ----

// taskPriority maps actionable buckets to an Obsidian Tasks priority emoji.
// Buckets missing from the map are informational and render as plain bullets.
var taskPriority = map[classify.Bucket]string{
	classify.ActNow: "🔺", classify.ReadyToEnqueue: "⏫", classify.ReviewRequested: "⏫",
	classify.ReReview: "🔼", classify.ReplyOwed: "🔼", classify.TeamOnly: "🔽",
}

func mdBuckets(w io.Writer, order []classify.Bucket, by map[classify.Bucket][]Item, o Options) {
	for _, b := range order {
		items := by[b]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n#### %s\n", titleCase(string(b)))
		for _, it := range items {
			label := ""
			if it.Res.Bot {
				label = "(" + it.Res.BotLabel + ") "
			}
			line := fmt.Sprintf("%s[%s](%s) %s: %s", label, it.PR.Ref(), it.PR.URL, mdEscape(it.PR.Title), it.Res.Reason)
			if o.stale(it) {
				line += " (stale)"
			}
			if it.Woke != "" {
				line += " (" + it.Woke + ")"
			}
			if prio, ok := taskPriority[b]; ok {
				fmt.Fprintf(w, "- [ ] %s %s\n", line, prio)
			} else {
				fmt.Fprintf(w, "- %s\n", line)
			}
		}
	}
}

// Markdown prints the report as Obsidian Tasks for a daily note. Actionable
// PRs are tasks with a priority; queued/waiting/draft PRs are plain bullets.
func Markdown(w io.Writer, items []Item, pokes []classify.PokeEntry, o Options) {
	v := group(items)
	fmt.Fprintf(w, "## PR triage (%s)\n", o.Now.Format("2006-01-02 15:04"))

	fmt.Fprintf(w, "\n### My pull requests\n")
	mdBuckets(w, classify.AuthoredOrder, v.mine, o)
	fmt.Fprintf(w, "\n### To review\n")
	mdBuckets(w, classify.ReviewOrder, v.review, o)

	if len(pokes) > 0 {
		fmt.Fprintf(w, "\n### Poke list\n\n")
		for _, p := range pokes {
			fmt.Fprintf(w, "- [ ] Poke %s about [%s](%s), idle %s 🔼\n", p.Holder.Who, p.Ref, p.URL, classify.FormatIdle(p.Holder.Idle))
			fmt.Fprintf(w, "    - `%s`\n", strings.ReplaceAll(Nudge(p, o), "`", "'"))
		}
	}
	if v.nBots > 0 {
		fmt.Fprintf(w, "\n### Automated\n")
		if o.ShowBots {
			mdBuckets(w, allBuckets, v.bots, o)
		} else {
			fmt.Fprintf(w, "\n%d bot PRs: %s\n", v.nBots, summary(v.bots))
		}
	}
	// Plain lines, not tasks: a snoozed PR needs no action until it wakes.
	switch {
	case len(v.snoozed) == 0:
	case !o.ShowSnoozed:
		noun := "PRs"
		if len(v.snoozed) == 1 {
			noun = "PR"
		}
		fmt.Fprintf(w, "\n%d snoozed %s hidden: %s\n", len(v.snoozed), noun, v.snoozedSummary())
	default:
		fmt.Fprintf(w, "\n### Snoozed\n\n")
		for _, it := range v.snoozed {
			fmt.Fprintf(w, "- [%s](%s) %s: %s, wakes %s\n", it.PR.Ref(), it.PR.URL, mdEscape(it.PR.Title), it.Res.Bucket, it.wakesAt())
		}
	}
}

func titleCase(s string) string {
	s = strings.ToLower(s)
	return strings.ToUpper(s[:1]) + s[1:]
}

func mdEscape(s string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`, "|", `\|`).Replace(s)
}
