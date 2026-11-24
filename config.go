package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/dbraley/gh-triage/internal/classify"
)

// Config is config.toml. Everything is optional; see config.example.toml.
type Config struct {
	Login               string   `toml:"login"`
	Teams               []string `toml:"teams"`
	ExcludeRepos        []string `toml:"exclude_repos"`
	ExcludeOrgs         []string `toml:"exclude_orgs"`
	TeamRequestsAreMine bool     `toml:"team_requests_are_mine"`
	Idle                struct {
		PokeAfterBusinessDays  float64 `toml:"poke_after_business_days"`
		StaleAfterBusinessDays float64 `toml:"stale_after_business_days"`
	} `toml:"idle"`
	Checks struct {
		AllWhenNoneRequired bool `toml:"all_when_none_required"`
	} `toml:"checks"`
	Bots struct {
		Show     bool                  `toml:"show"`
		Patterns []classify.BotPattern `toml:"patterns"`
	} `toml:"bots"`
	Slack struct {
		Handles map[string]string `toml:"handles"`
	} `toml:"slack"`
}

func defaultConfig() Config {
	var c Config
	c.Idle.PokeAfterBusinessDays = 1
	c.Idle.StaleAfterBusinessDays = 3
	c.Checks.AllWhenNoneRequired = true
	return c
}

// loadConfig reads path, or the first of ./config.toml and
// ~/.config/gh-triage/config.toml that exists. No file means defaults.
func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	candidates := []string{path}
	if path == "" {
		candidates = []string{"config.toml"}
		if dir, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(dir, ".config", "gh-triage", "config.toml"))
		}
	}
	for _, p := range candidates {
		md, err := toml.DecodeFile(p, &c)
		if errors.Is(err, fs.ErrNotExist) && path == "" {
			continue
		}
		if err != nil {
			return c, fmt.Errorf("%s: %w", p, err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return c, fmt.Errorf("%s: unknown setting %q", p, u[0].String())
		}
		break
	}
	handles := map[string]string{}
	for k, v := range c.Slack.Handles {
		handles[strings.ToLower(k)] = v
	}
	c.Slack.Handles = handles
	return c, nil
}

// excluded applies exclude_orgs / exclude_repos globs and skips archived repos.
func (c Config) excluded(nameWithOwner string, archived bool) bool {
	if archived {
		return true
	}
	owner, _, _ := strings.Cut(nameWithOwner, "/")
	for _, p := range c.ExcludeOrgs {
		if classify.Glob(p, owner) {
			return true
		}
	}
	for _, p := range c.ExcludeRepos {
		if classify.Glob(p, nameWithOwner) {
			return true
		}
	}
	return false
}

func businessDays(d float64) time.Duration { return time.Duration(d * 24 * float64(time.Hour)) }

func (c Config) classifyOptions(me string, teams []string, now time.Time) classify.Options {
	return classify.Options{
		Me: me, Teams: teams, Now: now,
		TeamRequestsAreMine:       c.TeamRequestsAreMine,
		PokeAfter:                 businessDays(c.Idle.PokeAfterBusinessDays),
		Bots:                      c.Bots.Patterns,
		AllChecksWhenNoneRequired: c.Checks.AllWhenNoneRequired,
	}
}
