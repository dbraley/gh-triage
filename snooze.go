package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
	"github.com/dbraley/gh-triage/internal/render"
	"github.com/dbraley/gh-triage/internal/snooze"
)

// itemCtx is the viewer and clock an item was classified with; its snooze is
// judged with the same ones.
type itemCtx struct {
	me  string
	now time.Time
}

const stateHelp = "snooze state `FILE` (default $XDG_STATE_HOME/gh-triage/snoozes.json, else ~/.local/state/gh-triage/snoozes.json)"

// connect builds the GitHub client and resolves who "me" is and their teams.
// teamsComplete is false when the team list could not be fully read: the team
// review searches then miss PRs, so absence from the report proves nothing.
func connect(cfg Config, stderr io.Writer) (client *fetch.Client, me string, teams []string, teamsComplete bool, err error) {
	if client, err = fetch.NewClient(); err != nil {
		return
	}
	if me = cfg.Login; me == "" {
		if me, err = client.Viewer(); err != nil {
			return
		}
	}
	teamsComplete = true
	if teams = cfg.Teams; len(teams) == 0 {
		if teams, err = client.Teams(me); err != nil {
			teamsComplete = false
			fmt.Fprintf(stderr, "warning: could not list all your teams (%v)\n  run `gh auth refresh -s read:org -s repo`, or set `teams` in config.toml\n", err)
			err = nil
		}
	}
	return
}

// canPrune decides whether snoozes of PRs missing from the report may be
// dropped: only on a live report where every lookup succeeded, since a PR
// that was never fetched looks exactly like a PR that closed.
func canPrune(live bool, fetched, fetchErrs int, teamsComplete bool) bool {
	return live && fetchErrs == 0 && fetched > 0 && teamsComplete
}

// onlyConfigAndState rejects report flags given before a subcommand, where
// they would otherwise be silently ignored.
func onlyConfigAndState(fs *flag.FlagSet, cmd string) error {
	var bad error
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "config" && f.Name != "state" && bad == nil {
			bad = fmt.Errorf("--%s does not apply to %s", f.Name, cmd)
		}
	})
	return bad
}

// parseInterleaved parses flags that may come before or after positional
// arguments (flag.Parse stops at the first positional).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil // everything after "--" is positional
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// targetKind says how a snooze/unsnooze argument names a PR.
type targetKind int

const (
	targetRef     targetKind = iota // a PR URL or owner/repo#N
	targetFixture                   // an existing fixture file
)

const targetForms = "a PR URL (https://github.com/org/repo/pull/N), org/repo#N, or a fixture file"

// classifyTarget works out what target names. Anything with "://" is a URL
// (unsnooze tolerates one it cannot parse, in case a hand-edited entry has
// it); anything else must be org/repo#N or an existing file.
func classifyTarget(cmd, target string) (targetKind, error) {
	switch {
	case snooze.IsShortRef(target):
		return targetRef, nil
	case strings.Contains(target, "://"):
		if _, ok := snooze.Canonical(target); !ok && cmd == "snooze" {
			return 0, fmt.Errorf("%q is not a PR URL: want %s", target, targetForms)
		}
		return targetRef, nil
	}
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() {
		return targetFixture, nil
	}
	return 0, fmt.Errorf("%q is neither a PR URL nor an existing file: want %s", target, targetForms)
}

// runSnoozeCmd handles `snooze <PR>` and `unsnooze <PR>`.
func runSnoozeCmd(cmd string, args []string, configPath, statePath string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gh-triage "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", configPath, "config `FILE`")
	fs.StringVar(&statePath, "state", statePath, stateHelp)
	forFlag := new(string)
	usage := "usage: gh triage unsnooze <PR url>"
	if cmd == "snooze" {
		forFlag = fs.String("for", "", "snooze for this much business time, e.g. 3d, 12h, 1d12h (default from config)")
		usage = "usage: gh triage snooze <PR url> [--for 3d]"
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s\n  <PR url> is %s\n\nflags:\n", usage, targetForms)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if len(pos) != 1 {
		return errors.New(usage)
	}
	forSet := false
	fs.Visit(func(f *flag.Flag) { forSet = forSet || f.Name == "for" })
	target := pos[0]

	kind, err := classifyTarget(cmd, target)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if kind == targetFixture && statePath == "" {
		return fmt.Errorf("%s: a fixture needs --state FILE (a fixture must never touch your real snoozes)", cmd)
	}
	if statePath == "" {
		if statePath, err = snooze.DefaultPath(); err != nil {
			return err
		}
	}
	store, err := snooze.Load(statePath)
	if err != nil {
		return fmt.Errorf("%s: %w", statePath, err)
	}

	if cmd == "unsnooze" {
		ref := target
		if kind == targetFixture {
			f, err := fetch.LoadFixture(target)
			if err != nil {
				return err
			}
			ref = f.Node.URL
		}
		e, ok := store.Get(ref)
		if !ok {
			return fmt.Errorf("%s is not snoozed", ref)
		}
		store.Delete(ref)
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "unsnoozed %s\n", e.URL)
		return nil
	}

	d := businessDays(cfg.Snooze.DefaultBusinessDays)
	if forSet {
		if d, err = snooze.ParseFor(*forFlag); err != nil {
			return err
		}
	} else if d <= 0 {
		return errors.New("snooze.default_business_days must be positive (or pass --for)")
	}
	pr, res, now, err := snoozeTarget(kind, target, cfg, stderr)
	if err != nil {
		return err
	}
	e := snooze.Entry{URL: pr.URL, SnoozedAt: now, Until: snooze.Until(now, d), HeadOID: pr.Head().OID, Bucket: res.Bucket, Causes: res.Causes}
	store.Put(e)
	if err := store.Save(); err != nil {
		return err
	}
	// Say what state was snoozed: the PR may have changed since it was last seen.
	fmt.Fprintf(stdout, "snoozed %s until %s, or earlier on %s\n  it is %s: %s\n",
		pr.Ref(), e.Until.Format(render.TimeFormat), render.WakeSummary, res.Bucket, res.Reason)
	return nil
}

// snoozeTarget classifies the PR to snooze: live from a URL, or offline from
// a fixture file, as --explain does.
func snoozeTarget(kind targetKind, target string, cfg Config, stderr io.Writer) (pr fetch.PR, res classify.Result, now time.Time, err error) {
	if kind == targetFixture {
		f, err := fetch.LoadFixture(target)
		if err != nil {
			return pr, res, now, err
		}
		return f.Node, classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt)), f.FetchedAt, nil
	}
	if canon, ok := snooze.Canonical(target); ok {
		target = canon // GitHub resolves the bare PR URL, not /files or ?query
	}
	client, me, teams, _, err := connect(cfg, stderr)
	if err != nil {
		return pr, res, now, err
	}
	ref, err := client.Resolve(target)
	if err != nil {
		return pr, res, now, err
	}
	f, err := client.PR(ref.ID)
	if err != nil {
		return pr, res, now, err
	}
	now = time.Now()
	return f.PR, classify.Classify(f.PR, cfg.classifyOptions(me, teams, now)), now, nil
}

// wakeNote words an Evaluate reason for a report line.
func wakeNote(reason string) string {
	if strings.HasPrefix(reason, "snooze ") { // "snooze expired"
		return reason
	}
	return "snooze ended: " + reason
}

// judge sets the snooze fields on it from its store entry, if any, and
// reports whether the snooze just ended.
func judge(store *snooze.Store, it *render.Item, c itemCtx, bots []classify.BotPattern) (woke bool) {
	e, ok := store.Get(it.PR.URL)
	if !ok {
		return false
	}
	snoozed, reason := snooze.Evaluate(e, it.PR, it.Res, snooze.Options{Me: c.me, Bots: bots, Now: c.now})
	if snoozed {
		it.SnoozedUntil = e.Until
		return false
	}
	it.Woke = wakeNote(reason)
	return true
}

// loadState opens the state file, or warns and returns nil: the report must
// not be lost to a bad state file, and a nil store is never saved over it.
func loadState(path string, stderr io.Writer) *snooze.Store {
	if path == "" {
		var err error
		if path, err = snooze.DefaultPath(); err != nil {
			fmt.Fprintf(stderr, "warning: snoozes ignored: %v\n", err)
			return nil
		}
	}
	store, err := snooze.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "warning: snoozes ignored: %s: %v\n", path, err)
		return nil
	}
	return store
}

// applyState marks snoozed items, clears entries that woke and, when prune is
// set, entries whose PR has left the report. It saves only if the store changed.
func applyState(path string, items []render.Item, ctxs []itemCtx, bots []classify.BotPattern, prune bool, stderr io.Writer) {
	store := loadState(path, stderr)
	if store == nil {
		return
	}
	changed := false
	open := map[string]bool{}
	for i := range items {
		open[snooze.Key(items[i].PR.URL)] = true
		if judge(store, &items[i], ctxs[i], bots) {
			store.Delete(items[i].PR.URL)
			changed = true
		}
	}
	if prune {
		for _, e := range store.Entries() {
			if !open[snooze.Key(e.URL)] {
				store.Delete(e.URL)
				changed = true
			}
		}
	}
	if changed {
		if err := store.Save(); err != nil {
			fmt.Fprintf(stderr, "warning: could not save snoozes: %v\n", err)
		}
	}
}

// explainSnooze annotates it for --explain. It never writes the store.
func explainSnooze(it *render.Item, c itemCtx, cfg Config, path string, stderr io.Writer) {
	if store := loadState(path, stderr); store != nil {
		judge(store, it, c, cfg.Bots.Patterns)
	}
}
