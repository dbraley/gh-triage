package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
	"github.com/dbraley/gh-triage/internal/render"
	"github.com/dbraley/gh-triage/internal/snooze"
)

// No test may touch the user's real snooze file: point the default path at a
// directory that is deleted afterwards.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-triage-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestExampleConfigParses(t *testing.T) {
	c, err := loadConfig("config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Idle.PokeAfterBusinessDays != 1 || !c.Checks.AllWhenNoneRequired || c.Bots.Show {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestExcluded(t *testing.T) {
	c := defaultConfig()
	c.ExcludeRepos = []string{"Org-1/archived-*"}
	c.ExcludeOrgs = []string{"old-*"}
	for _, tc := range []struct {
		repo     string
		archived bool
		want     bool
	}{
		{"org-1/repo-1", false, false},
		{"org-1/repo-1", true, true},
		{"org-1/archived-thing", false, true}, // pattern differs in case
		{"old-employer/site", false, true},
	} {
		if got := c.excluded(tc.repo, tc.archived); got != tc.want {
			t.Errorf("excluded(%q, %v) = %v", tc.repo, tc.archived, got)
		}
	}
}

// End to end over the committed fixtures: no network.
func TestRunFromFixtures(t *testing.T) {
	out := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		if err := run(append([]string{"--config", "config.example.toml"}, args...), &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
		return stdout.String()
	}

	term := out("--from", "testdata")
	for _, want := range []string{"ACT NOW (6)", "READY TO ENQUEUE (3)", "WAITING ON STACK (1)", "IN MERGE QUEUE (1)", "REVIEW REQUESTED (1)", "POKE (3)", "POKE LIST (4)", "AUTOMATED (2)", "--show-bots"} {
		if !strings.Contains(term, want) {
			t.Errorf("terminal output missing %q:\n%s", want, term)
		}
	}
	if strings.Contains(term, "[Dependabot]") {
		t.Error("bot PRs should be collapsed by default")
	}
	if !strings.Contains(out("--from", "testdata", "--show-bots"), "[Dependabot]") {
		t.Error("--show-bots should list bot PRs with their label")
	}

	md := out("--from", "testdata", "--markdown")
	for _, want := range []string{"- [ ] [org-1/repo-1#", "🔺", "### Poke list", "- [ ] Poke user-3"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown output missing %q:\n%s", want, md)
		}
	}

	trail := out("--explain", "testdata/authored_optional_check_pending.json")
	for _, want := range []string{"optional pending (ignored): ci/legacy, coverage-report", "POKE:"} {
		if !strings.Contains(trail, want) {
			t.Errorf("explain output missing %q:\n%s", want, trail)
		}
	}
}

// ---- snooze ----

// Fixtures used by the snooze tests; every committed fixture has a unique URL
// (TestFixtureURLsUnique), so each has its own snooze entry.
const (
	pokeFixture = "testdata/authored_optional_check_pending.json" // POKE, org-1/repo-1#101
	pokeURL     = "https://github.com/org-1/repo-1/pull/101"
	failFixture = "testdata/authored_required_failed.json" // ACT NOW, org-1/repo-1#106
	failURL     = "https://github.com/org-1/repo-1/pull/106"
	botFixture  = "testdata/bot_dependabot.json" // bot, org-1/repo-1#119

	// What failFixture classifies as; entries snoozed from it record this.
	failCauseKey = classify.CauseChecksFailed

	activityFixture = "testdata/authored_snooze_activity.json" // POKE, org-1/repo-1#121; comment by user-3 and review by user-4
	activityURL     = "https://github.com/org-1/repo-1/pull/121"
)

// runCLI runs the CLI against the example config.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = run(append([]string{"--config", "config.example.toml"}, args...), &o, &e)
	return o.String(), e.String(), err
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	o, e, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, e)
	}
	return o
}

func statePath(t *testing.T) string { return filepath.Join(t.TempDir(), "snoozes.json") }

func loadStore(t *testing.T, path string) *snooze.Store {
	t.Helper()
	s, err := snooze.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureClock(t *testing.T, path string) time.Time {
	t.Helper()
	f, err := fetch.LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	return f.FetchedAt
}

// pokeSection returns the poke list section.
func pokeSection(out string) string {
	_, after, _ := strings.Cut(out, "POKE LIST")
	after, _, _ = strings.Cut(after, "AUTOMATED")
	return after
}

func TestSnoozeHidesAndUnsnoozeRestores(t *testing.T) {
	st := statePath(t)
	base := mustRun(t, "--from", "testdata", "--state", st)
	if !strings.Contains(base, "POKE (3)") || !strings.Contains(base, "POKE LIST (4)") || !strings.Contains(pokeSection(base), "org-1/repo-1#101") {
		t.Fatalf("baseline changed:\n%s", base)
	}

	msg := mustRun(t, "snooze", pokeFixture, "--state", st)
	// The confirmation says which state was snoozed, and what wakes it.
	for _, want := range []string{"snoozed org-1/repo-1#101 until 2026-11-23 17:00 UTC, or earlier on " + render.WakeSummary, "it is POKE: required checks green, waiting on review from user-2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirmation missing %q:\n%s", want, msg)
		}
	}

	out := mustRun(t, "--from", "testdata", "--state", st)
	for _, want := range []string{"POKE (2)", "POKE LIST (3)", "SNOOZED (1)", "1 poke", "--show-snoozed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "org-1/repo-1#101") {
		t.Errorf("snoozed PR still shown:\n%s", out)
	}
	if !strings.Contains(out, "org-1/repo-1#104") {
		t.Error("other PR in the same bucket disappeared")
	}

	out = mustRun(t, "--from", "testdata", "--state", st, "--show-snoozed")
	for _, want := range []string{"SNOOZED (1)", "org-1/repo-1#101", "POKE, wakes 2026-11-23 17:00 UTC", "(or on " + render.WakeSummary + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("--show-snoozed missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(pokeSection(out), "#101") {
		t.Error("expanded snoozed PR leaked into the poke list")
	}

	if got := mustRun(t, "unsnooze", pokeURL, "--state", st); !strings.Contains(got, "unsnoozed") {
		t.Errorf("unsnooze output = %q", got)
	}
	if got := mustRun(t, "--from", "testdata", "--state", st); got != base {
		t.Errorf("report after unsnooze differs from baseline:\n%s", got)
	}
	if _, _, err := runCLI(t, "unsnooze", pokeURL, "--state", st); err == nil || !strings.Contains(err.Error(), "not snoozed") {
		t.Errorf("second unsnooze err = %v", err)
	}
}

func TestSnoozeDuration(t *testing.T) {
	now := fixtureClock(t, failFixture)
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{nil, businessDays(3)}, // [snooze] default in the example config
		{[]string{"--for", "12h"}, 12 * time.Hour},
		{[]string{"--for", "1d12h"}, 36 * time.Hour},
	} {
		st := statePath(t)
		mustRun(t, append([]string{"snooze", failFixture, "--state", st}, tc.args...)...)
		e, ok := loadStore(t, st).Get(failURL)
		if !ok {
			t.Fatal("entry not saved")
		}
		if want := snooze.Until(now, tc.want); !e.Until.Equal(want) || !e.SnoozedAt.Equal(now) || e.Bucket != classify.ActNow || e.HeadOID == "" {
			t.Errorf("%v: entry = %+v, want until %v", tc.args, e, want)
		}
	}
}

func TestSnoozeFlagOrder(t *testing.T) {
	var untils []time.Time
	for _, args := range [][]string{
		{"snooze", failFixture, "--for", "2d"},
		{"snooze", "--for", "2d", failFixture},
		{"snooze", "--for=2d", failFixture},
		{"snooze", "--", failFixture, "--for"}, // after "--" everything is positional: too many
	} {
		st := statePath(t)
		// --config and --state work after the subcommand too, at either end.
		full := append(append([]string{}, args...), "--state", st, "--config", "config.example.toml")
		var o, e bytes.Buffer
		err := run(full, &o, &e)
		if args[1] == "--" {
			if err == nil {
				t.Error("positional-only args should be a usage error")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, e.String())
		}
		entry, ok := loadStore(t, st).Get(failURL)
		if !ok {
			t.Fatalf("%v: nothing saved", args)
		}
		untils = append(untils, entry.Until)
	}
	for _, u := range untils {
		if !u.Equal(untils[0]) || !u.Equal(snooze.Until(fixtureClock(t, failFixture), businessDays(2))) {
			t.Errorf("until = %v, want all equal to 2 business days", untils)
		}
	}

	// Flags before the subcommand are accepted for --config/--state only.
	st := statePath(t)
	if _, e, err := runCLI(t, "--state", st, "snooze", failFixture); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if _, ok := loadStore(t, st).Get(failURL); !ok {
		t.Error("--state before the subcommand was ignored")
	}
}

// Report flags before a subcommand would be silently ignored, so each is an
// error. Only --config and --state mean the same on both sides.
func TestSnoozeFlagsBeforeSubcommandRejected(t *testing.T) {
	st := statePath(t)
	for _, flags := range [][]string{
		{"--markdown"}, {"--show-bots"}, {"--show-snoozed"},
		{"--dump", t.TempDir()}, {"--explain", failFixture}, {"--from", "testdata"}, {"--scrub", failFixture},
	} {
		for _, cmd := range []string{"snooze", "unsnooze"} {
			args := append(append(append([]string{}, flags...), cmd, failFixture), "--state", st)
			_, _, err := runCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), "does not apply to "+cmd) {
				t.Errorf("%v: err = %v, want %q", args, err, "does not apply to "+cmd)
			}
		}
	}
	if _, statErr := os.Stat(st); statErr == nil {
		t.Error("rejected commands must not create the state file")
	}
}

func TestSnoozeUsageErrors(t *testing.T) {
	st := statePath(t)
	for _, args := range [][]string{
		{"snooze", "--state", st},
		{"snooze", failFixture, pokeFixture, "--state", st},
		{"unsnooze", "--state", st},
		{"unsnooze", pokeURL, failURL, "--state", st},
		{"snooze", failFixture, "--for", "soon", "--state", st},
		{"snooze", failFixture, "--for", "0d", "--state", st},
		{"snooze", failFixture, "--for=", "--state", st}, // explicitly empty is not "use the default"
		{"snooze", failFixture, "--for", "", "--state", st},
		{"unsnooze", pokeURL, "--for", "3d", "--state", st}, // --for is snooze-only
		{"snooze", "testdata/does-not-exist.json", "--state", st},
		{"bogus"},
	} {
		if _, _, err := runCLI(t, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
	_, _, err := runCLI(t, "snooze", "--state", st)
	if err == nil || !strings.Contains(err.Error(), "usage: gh triage snooze <PR url>") {
		t.Errorf("missing argument error = %v", err)
	}
	if _, statErr := os.Stat(st); statErr == nil {
		t.Error("failed commands must not create the state file")
	}
}

// A target that is neither a PR reference nor a file names both forms.
func TestSnoozeBadTargetNamesAcceptedForms(t *testing.T) {
	st := statePath(t)
	for _, cmd := range []string{"snooze", "unsnooze"} {
		for _, target := range []string{"github.com/org-1/repo-1/pull/1", "org-1/repo-1/pull/1", "nonsense"} {
			_, _, err := runCLI(t, cmd, target, "--state", st)
			if err == nil || strings.Contains(err.Error(), "no such file") ||
				!strings.Contains(err.Error(), "https://github.com/org/repo/pull/N") || !strings.Contains(err.Error(), "fixture file") {
				t.Errorf("%s %s: err = %v, want one naming a PR URL and a fixture file", cmd, target, err)
			}
		}
	}
	// An http(s) URL that is not a PR is rejected by snooze before any network use.
	if _, _, err := runCLI(t, "snooze", "https://github.com/org-1/repo-1/issues/1", "--state", st); err == nil || !strings.Contains(err.Error(), "not a PR URL") {
		t.Errorf("issue URL err = %v", err)
	}
}

// A fixture must never touch the default state file: --state is required.
func TestFixtureSnoozeNeedsState(t *testing.T) {
	def, err := snooze.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"snooze", failFixture}, {"unsnooze", failFixture}, {"snooze", failFixture, "--for", "1d"}} {
		_, _, err := runCLI(t, args...)
		if err == nil || !strings.Contains(err.Error(), "a fixture needs --state FILE") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if _, statErr := os.Stat(def); statErr == nil {
		t.Errorf("default state file %s was created", def)
	}
}

func TestSnoozeHelpShowsSynopsis(t *testing.T) {
	for cmd, want := range map[string]string{"snooze": "usage: gh triage snooze <PR url> [--for 3d]", "unsnooze": "usage: gh triage unsnooze <PR url>"} {
		_, stderr, err := runCLI(t, cmd, "--help")
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{want, "org/repo#N", "~/.local/state/gh-triage/snoozes.json"} {
			if !strings.Contains(stderr, w) {
				t.Errorf("%s --help missing %q:\n%s", cmd, w, stderr)
			}
		}
	}
}

func TestResnoozeReplaces(t *testing.T) {
	st := statePath(t)
	now := fixtureClock(t, failFixture)
	mustRun(t, "snooze", failFixture, "--for", "1d", "--state", st)
	mustRun(t, "snooze", failFixture, "--for", "5d", "--state", st)
	s := loadStore(t, st)
	if n := len(s.Entries()); n != 1 {
		t.Fatalf("%d entries after re-snoozing, want 1", n)
	}
	if e, _ := s.Get(failURL); !e.Until.Equal(snooze.Until(now, 5*24*time.Hour)) {
		t.Errorf("Until = %v, want the 5d wake time", e.Until)
	}
}

// unsnooze finds what snooze stored whichever way the PR is named.
func TestUnsnoozeTargets(t *testing.T) {
	for _, target := range []string{
		failURL,
		failURL + "/",
		failURL + "/files",
		failURL + "/files/0123abc",
		failURL + "?diff=split",
		failURL + "#issuecomment-1",
		"http://github.com/org-1/repo-1/pull/106",
		"HTTPS://GITHUB.COM/ORG-1/REPO-1/PULL/106",
		"org-1/repo-1#106",
		failFixture, // a fixture file names its PR
	} {
		t.Run(target, func(t *testing.T) {
			st := statePath(t)
			mustRun(t, "snooze", failFixture, "--state", st)
			mustRun(t, "snooze", pokeFixture, "--state", st)
			out := mustRun(t, "unsnooze", target, "--state", st)
			if !strings.Contains(out, "unsnoozed "+failURL) {
				t.Errorf("output = %q", out)
			}
			s := loadStore(t, st)
			if _, ok := s.Get(failURL); ok {
				t.Error("entry not removed")
			}
			if _, ok := s.Get(pokeURL); !ok {
				t.Error("another PR's entry was removed")
			}
		})
	}
	// Different PRs do not match.
	st := statePath(t)
	mustRun(t, "snooze", failFixture, "--state", st)
	for _, target := range []string{"https://github.com/org-1/repo-1/pull/16", "org-1/repo-1#1060", "org-1/repo-2#106"} {
		if _, _, err := runCLI(t, "unsnooze", target, "--state", st); err == nil || !strings.Contains(err.Error(), "is not snoozed") {
			t.Errorf("unsnooze %s: err = %v", target, err)
		}
	}
}

// Without --state, snooze commands use the default path (here a temp dir).
func TestDefaultStatePathIsUsed(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	def := filepath.Join(xdg, "gh-triage", "snoozes.json")
	now := fixtureClock(t, failFixture)
	s := loadStore(t, def)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.ActNow, Causes: []string{failCauseKey}})
	s.Put(snooze.Entry{URL: pokeURL, SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.Poke})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// loadState and applyState with no path read the default file.
	if loadState("", io.Discard) == nil {
		t.Fatal("loadState(\"\") did not open the default file")
	}
	f, _ := fetch.LoadFixture(failFixture)
	cfg := defaultConfig()
	res := classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt))
	items := []render.Item{{PR: f.Node, Res: res}}
	applyState("", items, []itemCtx{{me: f.Viewer, now: f.FetchedAt}}, nil, true, io.Discard)
	if items[0].SnoozedUntil.IsZero() {
		t.Error("the default state file was not applied")
	}
	if after := loadStore(t, def); len(after.Entries()) != 1 {
		t.Errorf("prune through the default path left %d entries, want 1", len(after.Entries()))
	}

	// unsnooze with no --state removes from the default file.
	if out := mustRun(t, "unsnooze", failURL); !strings.Contains(out, "unsnoozed") {
		t.Errorf("output = %q", out)
	}
	if after := loadStore(t, def); len(after.Entries()) != 0 {
		t.Errorf("entries left: %+v", after.Entries())
	}
	if _, _, err := runCLI(t, "unsnooze", failURL); err == nil {
		t.Error("second unsnooze should fail")
	}
}

func TestSnoozeWakesOnConversationActivity(t *testing.T) {
	// Snoozed after the push (2026-11-16T15:00Z), the comment (11-17T12:00Z)
	// and the review (11-17T14:00Z) are later. They flow through real JSON.
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		name      string
		snoozedAt time.Time
		want      string
	}{
		{"comment first", at("2026-11-17T00:00:00Z"), "snooze ended: new comment by user-3"},
		{"only the review is later", at("2026-11-17T13:00:00Z"), "snooze ended: new review by user-4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := statePath(t)
			s := loadStore(t, st)
			s.Put(snooze.Entry{URL: activityURL, SnoozedAt: tc.snoozedAt, Until: tc.snoozedAt.Add(14 * 24 * time.Hour), HeadOID: "aaa111", Bucket: classify.Poke})
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			out := mustRun(t, "--from", "testdata", "--state", st)
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "org-1/repo-1#121") {
				t.Errorf("want #121 shown with %q:\n%s", tc.want, out)
			}
		})
	}
	// Snoozed after both: nothing to wake it.
	st := statePath(t)
	s := loadStore(t, st)
	late := at("2026-11-18T00:00:00Z")
	s.Put(snooze.Entry{URL: activityURL, SnoozedAt: late, Until: late.Add(72 * time.Hour), HeadOID: "aaa111", Bucket: classify.Poke})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, "--from", "testdata", "--state", st); strings.Contains(out, "org-1/repo-1#121") || !strings.Contains(out, "SNOOZED (1)") {
		t.Errorf("#121 should stay snoozed:\n%s", out)
	}
}

func TestFixtureURLsUnique(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	seen := map[string]string{}
	for _, p := range paths {
		f, err := fetch.LoadFixture(p)
		if err != nil {
			t.Fatal(err)
		}
		k := snooze.Key(f.Node.URL)
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s share the URL %s (and so a snooze entry)", p, other, f.Node.URL)
		}
		seen[k] = p
	}
}

func TestCanPrune(t *testing.T) {
	for _, tc := range []struct {
		name          string
		live          bool
		fetched, errs int
		teamsComplete bool
		want          bool
	}{
		{"live, everything fetched", true, 5, 0, true, true},
		{"fixture run", false, 5, 0, true, false},
		{"a PR fetch failed", true, 5, 1, true, false},
		{"every fetch failed", true, 0, 5, true, false},
		{"nothing fetched", true, 0, 0, true, false},
		{"team list incomplete", true, 5, 0, false, false},
		{"incomplete teams and a failure", true, 5, 2, false, false},
	} {
		if got := canPrune(tc.live, tc.fetched, tc.errs, tc.teamsComplete); got != tc.want {
			t.Errorf("%s: canPrune = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSnoozeWakes(t *testing.T) {
	now := fixtureClock(t, pokeFixture)
	for _, tc := range []struct {
		name  string
		entry snooze.Entry
		note  string
	}{
		{"expired", snooze.Entry{URL: pokeURL, SnoozedAt: now.Add(-72 * time.Hour), Until: now.Add(-time.Hour), Bucket: classify.Poke}, "snooze expired"},
		{"bucket change", snooze.Entry{URL: pokeURL, SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.ActNow}, "snooze ended: now POKE (was ACT NOW)"},
		{"new push", snooze.Entry{URL: pokeURL, SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.Poke, HeadOID: "other-oid"}, "snooze ended: new push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := statePath(t)
			s := loadStore(t, st)
			s.Put(tc.entry)
			s.Put(snooze.Entry{URL: failURL, SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.ActNow, Causes: []string{failCauseKey}}) // stays snoozed
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			out := mustRun(t, "--from", "testdata", "--state", st)
			if !strings.Contains(out, "org-1/repo-1#101") || !strings.Contains(out, tc.note) {
				t.Errorf("want #101 shown with %q:\n%s", tc.note, out)
			}
			if !strings.Contains(out, "SNOOZED (1)") || strings.Contains(out, "org-1/repo-1#106") {
				t.Errorf("the other entry should stay snoozed:\n%s", out)
			}
			after := loadStore(t, st)
			if _, ok := after.Get(pokeURL); ok {
				t.Error("woken entry should be removed from the file")
			}
			if _, ok := after.Get(failURL); !ok {
				t.Error("entry still snoozed should stay in the file")
			}
			if again := mustRun(t, "--from", "testdata", "--state", st); strings.Contains(again, tc.note) {
				t.Error("the note should show once")
			}
		})
	}
}

func TestSnoozeWakesOnHumanComment(t *testing.T) {
	// The thread-activity fixture has a human reviewer comment; snooze from
	// before it was written.
	const path = "testdata/authored_thread_activity.json"
	now := fixtureClock(t, path)
	f, _ := fetch.LoadFixture(path)
	st := statePath(t)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: f.Node.URL, SnoozedAt: now.Add(-30 * 24 * time.Hour), Until: now.Add(time.Hour), HeadOID: f.Node.Head().OID, Bucket: classify.ActNow, Causes: []string{classify.CauseThreads}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, "--from", "testdata", "--state", st)
	if !strings.Contains(out, "snooze ended: new comment by user-2") {
		t.Errorf("expected a wake on the reviewer's comment:\n%s", out)
	}
}

func TestSnoozedBotCountsAsSnoozed(t *testing.T) {
	st := statePath(t)
	mustRun(t, "snooze", botFixture, "--state", st)
	out := mustRun(t, "--from", "testdata", "--state", st)
	for _, want := range []string{"SNOOZED (1)", "AUTOMATED (1)", "POKE LIST (3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(pokeSection(out), "#119") {
		t.Error("snoozed bot PR still on the poke list")
	}
}

func TestSnoozeMarkdown(t *testing.T) {
	st := statePath(t)
	mustRun(t, "snooze", failFixture, "--state", st)
	md := mustRun(t, "--from", "testdata", "--state", st, "--markdown")
	if strings.Contains(md, "#106") {
		t.Errorf("snoozed PR in markdown:\n%s", md)
	}
	if !strings.Contains(md, "\n1 snoozed PR hidden: 1 act now\n") {
		t.Errorf("missing count line:\n%s", md)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "snoozed") && strings.HasPrefix(line, "- ") {
			t.Errorf("snoozed line must not be a task: %q", line)
		}
	}

	// A woken PR carries its note in the markdown too.
	now := fixtureClock(t, failFixture)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: now.Add(-48 * time.Hour), Until: now.Add(-time.Hour), Bucket: classify.ActNow})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	md = mustRun(t, "--from", "testdata", "--state", st, "--markdown")
	if !strings.Contains(md, "#106") || !strings.Contains(md, "(snooze expired)") || strings.Contains(md, "snoozed PRs hidden") {
		t.Errorf("woken PR in markdown:\n%s", md)
	}
}

func TestFromIgnoresDefaultState(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	def, err := snooze.DefaultPath()
	if err != nil || !strings.HasPrefix(def, xdg) {
		t.Fatalf("DefaultPath = %q, %v", def, err)
	}
	s := loadStore(t, def)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: fixtureClock(t, failFixture), Until: fixtureClock(t, failFixture).Add(72 * time.Hour), Bucket: classify.ActNow, Causes: []string{failCauseKey}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(def)

	out := mustRun(t, "--from", "testdata")
	if strings.Contains(out, "SNOOZED") || !strings.Contains(out, "org-1/repo-1#106") {
		t.Errorf("--from without --state read the default state file:\n%s", out)
	}
	if ex := mustRun(t, "--explain", failFixture); strings.Contains(ex, "Snoozed") {
		t.Errorf("fixture --explain without --state read the default state file:\n%s", ex)
	}
	if out := mustRun(t, "--from", "testdata", "--state", def); !strings.Contains(out, "SNOOZED (1)") {
		t.Errorf("explicit --state should apply:\n%s", out)
	}
	if after, _ := os.ReadFile(def); !bytes.Equal(before, after) {
		t.Error("state file changed")
	}
}

func TestFromNeverPrunes(t *testing.T) {
	st := statePath(t)
	s := loadStore(t, st)
	now := fixtureClock(t, failFixture)
	s.Put(snooze.Entry{URL: "https://github.com/org-1/repo-1/pull/999", SnoozedAt: now, Until: now.Add(72 * time.Hour), Bucket: classify.ActNow})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "--from", "testdata", "--state", st)
	if _, ok := loadStore(t, st).Get("https://github.com/org-1/repo-1/pull/999"); !ok {
		t.Error("--from pruned an entry for a PR it never fetched")
	}
}

func TestExplainShowsSnoozeWithoutWriting(t *testing.T) {
	st := statePath(t)
	mustRun(t, "snooze", failFixture, "--state", st)
	out := mustRun(t, "--explain", failFixture, "--state", st)
	if !strings.Contains(out, "Snoozed until ") || !strings.Contains(out, render.WakeSummary) {
		t.Errorf("explain missing snooze status:\n%s", out)
	}

	now := fixtureClock(t, failFixture)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: now.Add(-48 * time.Hour), Until: now.Add(-time.Hour), Bucket: classify.ActNow})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(st)
	out = mustRun(t, "--explain", failFixture, "--state", st)
	if !strings.Contains(out, "Snooze would wake now: snooze expired") {
		t.Errorf("explain missing wake reason:\n%s", out)
	}
	if after, _ := os.ReadFile(st); !bytes.Equal(before, after) {
		t.Error("--explain modified the store")
	}
	if out := mustRun(t, "--explain", pokeFixture, "--state", st); strings.Contains(out, "nooze") {
		t.Errorf("explain of an unsnoozed PR mentions snooze:\n%s", out)
	}
}

func TestBadStateDoesNotLoseReport(t *testing.T) {
	st := statePath(t)
	if err := os.WriteFile(st, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, "--from", "testdata", "--state", st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ACT NOW (6)") || !strings.Contains(errOut, "snoozes ignored") {
		t.Errorf("report should survive a bad state file with a warning; stderr=%q", errOut)
	}
	if b, _ := os.ReadFile(st); string(b) != "{not json" {
		t.Error("a bad state file must not be overwritten")
	}
	// snooze and unsnooze both fail, name the file and leave it alone.
	for _, cmd := range []string{"snooze", "unsnooze"} {
		_, _, err := runCLI(t, cmd, failFixture, "--state", st)
		if err == nil || !strings.Contains(err.Error(), st) {
			t.Errorf("%s with an unreadable state file: err = %v, want one naming %s", cmd, err, st)
		}
		if b, _ := os.ReadFile(st); string(b) != "{not json" {
			t.Errorf("%s modified a bad state file", cmd)
		}
	}
}

// A state file that cannot be written is a warning; the report still prints.
func TestSaveFailureWarnsAndKeepsReport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := filepath.Join(dir, "snoozes.json")
	now := fixtureClock(t, failFixture)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: now.Add(-48 * time.Hour), Until: now.Add(-time.Hour), Bucket: classify.ActNow}) // expired: needs a rewrite
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	out, errOut, err := runCLI(t, "--from", "testdata", "--state", st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "could not save snoozes") || !strings.Contains(out, "ACT NOW (6)") {
		t.Errorf("want a warning and the report; stderr=%q", errOut)
	}
}

func TestSnoozeConfig(t *testing.T) {
	c, err := loadConfig("config.example.toml")
	if err != nil || c.Snooze.DefaultBusinessDays != 3 {
		t.Fatalf("example snooze config = %+v, %v", c.Snooze, err)
	}
	bad := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(bad, []byte("[snooze]\ndefault_days = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil || !strings.Contains(err.Error(), "snooze.default_days") {
		t.Errorf("unknown [snooze] key err = %v", err)
	}
	// A zero default means --for is required.
	zero := filepath.Join(t.TempDir(), "z.toml")
	if err := os.WriteFile(zero, []byte("[snooze]\ndefault_business_days = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := statePath(t)
	var o, e bytes.Buffer
	if err := run([]string{"snooze", failFixture, "--config", zero, "--state", st}, &o, &e); err == nil {
		t.Error("zero default without --for should fail")
	}
	if err := run([]string{"snooze", failFixture, "--for", "1d", "--config", zero, "--state", st}, &o, &e); err != nil {
		t.Errorf("--for should override a zero default: %v", err)
	}
}

// Pruning needs a live fetch, so exercise applyState directly.
func TestApplyStatePrune(t *testing.T) {
	f, err := fetch.LoadFixture(failFixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	res := classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt))
	items := []render.Item{{PR: f.Node, Res: res}}
	ctxs := []itemCtx{{me: f.Viewer, now: f.FetchedAt}}
	const gone = "https://github.com/org-1/repo-1/pull/998"

	for _, tc := range []struct {
		prune    bool
		wantGone bool
	}{{false, true}, {true, false}} {
		st := statePath(t)
		s := loadStore(t, st)
		s.Put(snooze.Entry{URL: gone, SnoozedAt: f.FetchedAt, Until: f.FetchedAt.Add(time.Hour), Bucket: classify.ActNow})
		// Stored under a different case and form than the report's URL.
		s.Put(snooze.Entry{URL: "HTTP://GitHub.com/Org-1/REPO-1/pull/106/files", SnoozedAt: f.FetchedAt, Until: f.FetchedAt.Add(time.Hour), HeadOID: f.Node.Head().OID, Bucket: res.Bucket, Causes: res.Causes})
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		its := append([]render.Item{}, items...)
		applyState(st, its, ctxs, nil, tc.prune, &warn)
		after := loadStore(t, st)
		if _, ok := after.Get(gone); ok != tc.wantGone {
			t.Errorf("prune=%v: closed PR kept = %v", tc.prune, ok)
		}
		if _, ok := after.Get(failURL); !ok || its[0].SnoozedUntil.IsZero() {
			t.Errorf("prune=%v: open snoozed PR lost its snooze", tc.prune)
		}
		if warn.Len() != 0 {
			t.Errorf("unexpected warning: %s", warn.String())
		}
	}
}

func stateFixture(t *testing.T, path string) (fetch.Fixture, classify.Result) {
	t.Helper()
	f, err := fetch.LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	return f, classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt))
}

// The viewer's own comments and a configured bot's never wake a snooze, so
// applyState must pass the item's viewer and the configured patterns.
func TestApplyStateIgnoresViewerAndBotActivity(t *testing.T) {
	f, res := stateFixture(t, failFixture)
	later := f.FetchedAt.Add(-time.Hour)
	f.Node.Comments.Nodes = []fetch.Comment{
		{Author: fetch.Actor{Typename: "User", Login: strings.ToUpper(f.Viewer)}, CreatedAt: later},
		{Author: fetch.Actor{Typename: "User", Login: "release-bot-1"}, CreatedAt: later},
	}
	f.Node.Reviews.Nodes = []fetch.Review{{Author: fetch.Actor{Typename: "User", Login: "release-bot-2"}, SubmittedAt: later}}
	bots := []classify.BotPattern{{Pattern: "release-bot-*", Label: "Release bot"}}

	st := statePath(t)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: f.FetchedAt.Add(-2 * time.Hour), Until: f.FetchedAt.Add(72 * time.Hour),
		HeadOID: f.Node.Head().OID, Bucket: res.Bucket, Causes: res.Causes})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(st)

	var warn bytes.Buffer
	items := []render.Item{{PR: f.Node, Res: res}}
	applyState(st, items, []itemCtx{{me: f.Viewer, now: f.FetchedAt}}, bots, false, &warn)
	if items[0].SnoozedUntil.IsZero() || items[0].Woke != "" {
		t.Errorf("item woke: %+v", items[0].Woke)
	}
	if after, _ := os.ReadFile(st); !bytes.Equal(before, after) {
		t.Error("state file changed although nothing woke")
	}

	// Without the bot pattern the same activity does wake it (the test is live).
	items = []render.Item{{PR: f.Node, Res: res}}
	applyState(st, items, []itemCtx{{me: f.Viewer, now: f.FetchedAt}}, nil, false, &warn)
	if !strings.Contains(items[0].Woke, "new comment by release-bot-1") {
		t.Errorf("Woke = %q", items[0].Woke)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning: %s", warn.String())
	}
}

// Each item is judged at its own clock.
func TestApplyStateJudgesEachItemAtItsOwnClock(t *testing.T) {
	f1, r1 := stateFixture(t, failFixture)
	f2, r2 := stateFixture(t, pokeFixture)
	t1 := f1.FetchedAt
	until := t1.Add(5 * 24 * time.Hour)
	st := statePath(t)
	s := loadStore(t, st)
	s.Put(snooze.Entry{URL: failURL, SnoozedAt: t1, Until: until, HeadOID: f1.Node.Head().OID, Bucket: r1.Bucket, Causes: r1.Causes})
	s.Put(snooze.Entry{URL: pokeURL, SnoozedAt: t1, Until: until, HeadOID: f2.Node.Head().OID, Bucket: r2.Bucket, Causes: r2.Causes})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	items := []render.Item{{PR: f1.Node, Res: r1}, {PR: f2.Node, Res: r2}}
	ctxs := []itemCtx{{me: f1.Viewer, now: t1}, {me: f2.Viewer, now: t1.Add(10 * 24 * time.Hour)}}
	applyState(st, items, ctxs, nil, false, io.Discard)
	if items[0].SnoozedUntil.IsZero() || items[0].Woke != "" {
		t.Errorf("first item (clock before Until) should be snoozed: %+v", items[0])
	}
	if !items[1].SnoozedUntil.IsZero() || items[1].Woke != "snooze expired" {
		t.Errorf("second item (clock after Until) Woke = %q, SnoozedUntil = %v", items[1].Woke, items[1].SnoozedUntil)
	}
	after := loadStore(t, st)
	if _, ok := after.Get(failURL); !ok {
		t.Error("snoozed entry removed")
	}
	if _, ok := after.Get(pokeURL); ok {
		t.Error("expired entry kept")
	}
}

func TestStateUnchangedIsNotRewritten(t *testing.T) {
	st := statePath(t)
	mustRun(t, "snooze", failFixture, "--state", st)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(st, old, old); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "--from", "testdata", "--state", st)
	if fi, _ := os.Stat(st); !fi.ModTime().Equal(old) {
		t.Error("state file rewritten although nothing changed")
	}
}
