package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
)

var t0 = time.Date(2026, 3, 4, 17, 0, 0, 0, time.UTC)

func item(n int, b classify.Bucket) Item {
	var pr fetch.PR
	pr.Number = n
	pr.Title = "Title " + string(rune('A'+n))
	pr.URL = "https://github.com/org-1/repo-1/pull/" + itoaTest(n)
	pr.Repository.NameWithOwner = "org-1/repo-1"
	return Item{PR: pr, Res: classify.Result{Bucket: b, Mine: true, Reason: "reason", SortTime: t0}}
}

func itoaTest(n int) string { return string(rune('0' + n)) }

func snoozed(it Item, until time.Time) Item { it.SnoozedUntil = until; return it }

func terminal(items []Item, o Options) string {
	var b bytes.Buffer
	Terminal(&b, items, nil, o)
	return b.String()
}

func TestTerminalSnoozed(t *testing.T) {
	bot := item(3, classify.ReadyToEnqueue)
	bot.Res.Bot, bot.Res.BotLabel = true, "Dependabot"
	items := []Item{
		item(1, classify.ActNow),
		snoozed(item(2, classify.ActNow), t0.Add(48*time.Hour)),
		snoozed(bot, t0.Add(24*time.Hour)),
	}
	o := Options{Now: t0}

	out := terminal(items, o)
	for _, want := range []string{"ACT NOW (1)", "MY PULL REQUESTS (1)", "SNOOZED (2)", "1 act now, 1 ready to enqueue", "(--show-snoozed to expand)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#2") || strings.Contains(out, "AUTOMATED") {
		t.Errorf("snoozed items must be hidden, and a snoozed bot is not Automated:\n%s", out)
	}

	o.ShowSnoozed = true
	out = terminal(items, o)
	// Earliest wake first.
	i3, i2 := strings.Index(out, "org-1/repo-1#3"), strings.Index(out, "org-1/repo-1#2")
	if i3 < 0 || i2 < 0 || i3 > i2 {
		t.Errorf("expanded list missing or misordered:\n%s", out)
	}
	for _, want := range []string{"ACT NOW, wakes 2026-03-06 17:00 UTC (or on a push, a comment or review by someone else, or a change in what it needs)", "READY TO ENQUEUE, wakes 2026-03-05 17:00 UTC"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestWokeNote(t *testing.T) {
	it := item(1, classify.ActNow)
	it.Woke = "snooze ended: new comment by user-3"
	if out := terminal([]Item{it}, Options{Now: t0}); !strings.Contains(out, "(snooze ended: new comment by user-3)") || strings.Contains(out, "SNOOZED") {
		t.Errorf("woke note:\n%s", out)
	}
	var md bytes.Buffer
	Markdown(&md, []Item{it}, nil, Options{Now: t0})
	if !strings.Contains(md.String(), "reason (snooze ended: new comment by user-3) 🔺") {
		t.Errorf("markdown woke note:\n%s", md.String())
	}
}

func TestMarkdownSnoozed(t *testing.T) {
	items := []Item{item(1, classify.ActNow), snoozed(item(2, classify.ActNow), t0.Add(time.Hour))}
	var b bytes.Buffer
	Markdown(&b, items, nil, Options{Now: t0})
	out := b.String()
	if strings.Contains(out, "#2") || !strings.Contains(out, "\n1 snoozed PR hidden: 1 act now\n") {
		t.Errorf("hidden form:\n%s", out)
	}
	b.Reset()
	Markdown(&b, append(items, snoozed(item(3, classify.Poke), t0.Add(time.Hour))), nil, Options{Now: t0})
	if !strings.Contains(b.String(), "\n2 snoozed PRs hidden: 1 act now, 1 poke\n") {
		t.Errorf("plural form:\n%s", b.String())
	}
	b.Reset()
	Markdown(&b, items, nil, Options{Now: t0, ShowSnoozed: true})
	out = b.String()
	if !strings.Contains(out, "wakes 2026-03-04 18:00 UTC (or on "+WakeSummary+")") ||
		!strings.Contains(out, "\n- [org-1/repo-1#2](https://github.com/org-1/repo-1/pull/2)") || strings.Contains(out, "- [ ] [org-1/repo-1#2]") {
		t.Errorf("expanded form:\n%s", out)
	}
}

func TestExplainSnooze(t *testing.T) {
	var b bytes.Buffer
	Explain(&b, snoozed(item(1, classify.ActNow), t0.Add(time.Hour)))
	if !strings.Contains(b.String(), "Snoozed until 2026-03-04 18:00 UTC (or on "+WakeSummary+")") {
		t.Errorf("snoozed explain:\n%s", b.String())
	}
	b.Reset()
	it := item(1, classify.ActNow)
	it.Woke = "snooze expired"
	Explain(&b, it)
	if !strings.Contains(b.String(), "Snooze would wake now: snooze expired") {
		t.Errorf("woke explain:\n%s", b.String())
	}
}
