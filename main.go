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
	dump := fs.String("dump", "", "save each PR's raw GraphQL response to `DIR` as a fixture (private data!)")
	explain := fs.String("explain", "", "print the decision trail for one PR `URL` (or fixture file)")
	from := fs.String("from", "", "classify the fixtures in `DIR` instead of querying GitHub")
	scrub := fs.String("scrub", "", "print a scrubbed, committable copy of fixture `FILE`")
	configPath := fs.String("config", "", "config `FILE` (default ./config.toml, then ~/.config/gh-triage/config.toml)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
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
	classifyFixture := func(f fetch.Fixture) render.Item {
		return render.Item{PR: f.Node, Res: classify.Classify(f.Node, cfg.classifyOptions(f.Viewer, f.ViewerTeams, f.FetchedAt))}
	}

	// Offline paths: a fixture carries its own viewer, teams and clock.
	if *explain != "" && !strings.Contains(*explain, "://") {
		f, err := fetch.LoadFixture(*explain)
		if err != nil {
			return err
		}
		render.Explain(stdout, classifyFixture(f))
		return nil
	}
	var items []render.Item
	var pokeOpts classify.Options
	if *from != "" {
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
		client, err := fetch.NewClient()
		if err != nil {
			return err
		}
		me := cfg.Login
		if me == "" {
			if me, err = client.Viewer(); err != nil {
				return err
			}
		}
		teams := cfg.Teams
		if len(teams) == 0 {
			if teams, err = client.Teams(me); err != nil {
				fmt.Fprintf(stderr, "warning: could not list all your teams (%v)\n  run `gh auth refresh -s read:org -s repo`, or set `teams` in config.toml\n", err)
			}
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
			render.Explain(stdout, render.Item{PR: f.PR, Res: classify.Classify(f.PR, copts)})
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
		}
		if *dump != "" {
			fmt.Fprintf(stderr, "wrote %d fixtures to %s (private: scrub before committing)\n", len(fetched), *dump)
		}
	}

	prs := make([]fetch.PR, len(items))
	results := make([]classify.Result, len(items))
	for i, it := range items {
		prs[i], results[i] = it.PR, it.Res
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
