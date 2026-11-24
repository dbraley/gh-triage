// gh-triage tells you which PRs need your action, which you owe a review on,
// and who to poke. State is computed from GitHub data, not GitHub's inbox.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/term"

	"github.com/dbraley/gh-triage/internal/classify"
	"github.com/dbraley/gh-triage/internal/fetch"
	"github.com/dbraley/gh-triage/internal/render"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gh-triage:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gh-triage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	markdown := fs.Bool("markdown", false, "print Obsidian Tasks markdown instead of the terminal report")
	showBots := fs.Bool("show-bots", false, "expand the Automated section")
	showSnoozed := fs.Bool("show-snoozed", false, "expand the Snoozed section")
	statePath := fs.String("state", "", stateHelp)
	dump := fs.String("dump", "", "save each PR's raw GraphQL response to `DIR` as a fixture (private data!)")
	explain := fs.String("explain", "", "print the decision trail for one PR `URL` (or fixture file)")
	from := fs.String("from", "", "classify the fixtures in `DIR` instead of querying GitHub")
	scrub := fs.String("scrub", "", "print a scrubbed, committable copy of fixture `FILE`")
	configPath := fs.String("config", "", "config `FILE` (default ./config.toml, then ~/.config/gh-triage/config.toml)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: gh triage [flags]\n       gh triage snooze <PR url> [--for 3d]\n       gh triage unsnooze <PR url>\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		cmd := fs.Arg(0)
		if cmd != "snooze" && cmd != "unsnooze" {
			return fmt.Errorf("unexpected argument %q", cmd)
		}
		if err := onlyConfigAndState(fs, cmd); err != nil {
			return err
		}
		return runSnoozeCmd(cmd, fs.Args()[1:], *configPath, *statePath, stdout, stderr)
	}

	if *scrub != "" {
		f, err := fetch.LoadFixture(*scrub)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(fetch.Scrub(f))
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ropts := render.Options{
		ShowBots:     *showBots || cfg.Bots.Show,
		Color:        term.FromEnv().IsColorEnabled(),
		Now:          time.Now(),
		StaleAfter:   businessDays(cfg.Idle.StaleAfterBusinessDays),
		SlackHandles: cfg.Slack.Handles,
	}
	// Each item is classified, and its snooze judged, at its own clock.
	var ctxs []itemCtx
	classifyFixture := func(f fetch.Fixture) render.Item {
		ctxs = append(ctxs, itemCtx{me: f.Viewer, now: f.FetchedAt})
		return render.Item{PR: f.Node, Res: classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt))}
	}
	live := *from == ""
	// Fixture runs must never read the user's real state file.
	useState := live || *statePath != ""

	// Offline paths: a fixture carries its own viewer, teams and clock.
	if *explain != "" && !strings.Contains(*explain, "://") {
		f, err := fetch.LoadFixture(*explain)
		if err != nil {
			return err
		}
		it := classifyFixture(f)
		if *statePath != "" { // a fixture never reads the default state file
			explainSnooze(&it, ctxs[0], cfg, *statePath, stderr)
		}
		render.Explain(stdout, it)
		return nil
	}
	var items []render.Item
	var pokeOpts classify.Options
	prune := false // every live lookup succeeded, so absence means the PR has left the report
	if !live {
		paths, err := filepath.Glob(filepath.Join(*from, "*.json"))
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return fmt.Errorf("no fixtures in %s", *from)
		}
		for _, p := range paths {
			f, err := fetch.LoadFixture(p)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			items = append(items, classifyFixture(f))
			ropts.Now = f.FetchedAt
		}
		pokeOpts = cfg.classifyOptions("", nil, ropts.Now)
	} else {
		client, me, teams, teamsComplete, err := connect(cfg, stderr)
		if err != nil {
			return err
		}
		copts := cfg.classifyOptions(me, teams, ropts.Now)
		pokeOpts = copts
		save := func(f fetch.Fetched) error {
			if *dump == "" {
				return nil
			}
			_, err := fetch.Dump(*dump, me, teams, ropts.Now, f)
			return err
		}

		if *explain != "" {
			ref, err := client.Resolve(*explain)
			if err != nil {
				return err
			}
			f, err := client.PR(ref.ID)
			if err != nil {
				return err
			}
			if err := save(f); err != nil {
				return err
			}
			it := render.Item{PR: f.PR, Res: classify.Classify(f.PR, copts)}
			explainSnooze(&it, itemCtx{me: me, now: ropts.Now}, cfg, *statePath, stderr)
			render.Explain(stdout, it)
			return nil
		}

		refs, err := search(client, me, teams)
		if err != nil {
			return err
		}
		var wanted []fetch.Ref
		for _, r := range refs {
			if !cfg.excluded(r.Repository.NameWithOwner, r.Repository.IsArchived) {
				wanted = append(wanted, r)
			}
		}
		fmt.Fprintf(stderr, "%d open PRs involve %s (%d excluded); fetching details...\n", len(wanted), me, len(refs)-len(wanted))
		fetched, errs := client.PRs(wanted)
		for _, e := range errs {
			fmt.Fprintln(stderr, "warning: skipped", e)
		}
		for _, f := range fetched {
			if err := save(f); err != nil {
				return err
			}
			items = append(items, render.Item{PR: f.PR, Res: classify.Classify(f.PR, copts)})
			ctxs = append(ctxs, itemCtx{me: me, now: ropts.Now})
		}
		// A transient error must not drop a snooze; an empty result is more
		// likely an outage than every PR having closed.
		prune = canPrune(live, len(fetched), len(errs), teamsComplete)
		if *dump != "" {
			fmt.Fprintf(stderr, "wrote %d fixtures to %s (private: scrub before committing)\n", len(fetched), *dump)
		}
	}

	if useState {
		applyState(*statePath, items, ctxs, cfg.Bots.Patterns, prune, stderr)
	}
	ropts.ShowSnoozed = *showSnoozed

	// Snoozed PRs are hidden, so nobody gets poked about them either.
	var prs []fetch.PR
	var results []classify.Result
	for _, it := range items {
		if it.SnoozedUntil.IsZero() {
			prs, results = append(prs, it.PR), append(results, it.Res)
		}
	}
	pokes := classify.PokeList(prs, results, pokeOpts)
	if *markdown {
		render.Markdown(stdout, items, pokes, ropts)
	} else {
		render.Terminal(stdout, items, pokes, ropts)
	}
	return nil
}

// search runs every "involves me" query and returns the union.
func search(c *fetch.Client, me string, teams []string) ([]fetch.Ref, error) {
	const base = "is:pr is:open archived:false "
	queries := []string{
		base + "author:" + me,
		base + "review-requested:" + me,
		base + "reviewed-by:" + me,
	}
	for _, t := range teams {
		queries = append(queries, base+"team-review-requested:"+t)
	}
	found := make([][]fetch.Ref, len(queries))
	errs := make([]error, len(queries))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			found[i], errs[i] = c.Search(q)
		}()
	}
	wg.Wait()
	var refs []fetch.Ref
	seen := map[string]bool{}
	for i := range queries {
		if errs[i] != nil {
			return nil, errs[i]
		}
		for _, r := range found[i] {
			if !seen[r.ID] {
				seen[r.ID] = true
				refs = append(refs, r)
			}
		}
	}
	return refs, nil
}
