package main

import (
	"bytes"
	"strings"
	"testing"
)

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
	c.ExcludeRepos = []string{"Khan/archived-*"}
	c.ExcludeOrgs = []string{"old-*"}
	for _, tc := range []struct {
		repo     string
		archived bool
		want     bool
	}{
		{"Khan/webapp", false, false},
		{"Khan/webapp", true, true},
		{"khan/archived-thing", false, true},
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
	for _, want := range []string{"ACT NOW (6)", "READY TO ENQUEUE (3)", "WAITING ON STACK (1)","IN MERGE QUEUE (1)", "REVIEW REQUESTED (1)", "POKE LIST (3)", "AUTOMATED (2)", "--show-bots"} {
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
