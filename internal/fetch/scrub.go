package fetch

import (
	"fmt"
	"strings"
)

// publicBots keep their login when scrubbed: they are not private, and the
// name is what bot detection keys on.
var publicBots = []string{"dependabot", "renovate", "github-actions", "github-merge-queue"}

// Scrub returns a copy of a fixture that is safe to commit: logins, orgs,
// repos, teams, titles, URLs, check names and commit ids are replaced with
// stable placeholders. The viewer becomes "me". States, flags and timestamps
// (everything classification depends on) are kept. Because the fixture has
// been decoded into typed structs, any field this tool does not read is
// already gone.
func Scrub(f Fixture) Fixture {
	s := &scrubber{maps: map[string]map[string]string{}, viewer: f.Viewer}
	pr := f.Node

	org := s.org(pr.Repository.Owner.Login)
	repo := org + "/" + s.name("repo", pr.Repository.NameWithOwner)
	pr.ID, pr.Number = "PR_scrubbed", 1
	pr.Title = "Scrubbed pull request"
	pr.URL = "https://github.com/" + repo + "/pull/1"
	pr.Repository.NameWithOwner = repo
	pr.Repository.Owner.Login = org
	s.actor(&pr.Author)

	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		c.OID = s.name("oid", c.OID)
		if c.StatusCheckRollup == nil {
			continue
		}
		for j := range c.StatusCheckRollup.Contexts.Nodes {
			ch := &c.StatusCheckRollup.Contexts.Nodes[j]
			if ch.Name != "" {
				ch.Name = s.name("check", ch.Name)
			}
			if ch.Context != "" {
				ch.Context = s.name("check", ch.Context)
			}
		}
	}
	for _, reviews := range [][]Review{pr.LatestOpinionatedReviews.Nodes, pr.LatestReviews.Nodes} {
		for i := range reviews {
			s.actor(&reviews[i].Author)
			if reviews[i].Commit != nil {
				reviews[i].Commit.OID = s.name("oid", reviews[i].Commit.OID)
			}
		}
	}
	for i := range pr.ReviewRequests.Nodes {
		s.reviewer(&pr.ReviewRequests.Nodes[i].RequestedReviewer)
	}
	for i := range pr.ReviewThreads.Nodes {
		cs := pr.ReviewThreads.Nodes[i].Comments.Nodes
		for j := range cs {
			s.actor(&cs[j].Author)
		}
	}
	for i := range pr.TimelineItems.Nodes {
		ev := &pr.TimelineItems.Nodes[i]
		if ev.RequestedReviewer != nil {
			s.reviewer(ev.RequestedReviewer)
		}
		if ev.Actor != nil {
			s.actor(ev.Actor)
		}
	}

	out := Fixture{FetchedAt: f.FetchedAt, Viewer: "me", Node: pr}
	for _, t := range f.ViewerTeams {
		o, slug, _ := strings.Cut(t, "/")
		out.ViewerTeams = append(out.ViewerTeams, s.team(o, slug))
	}
	return out
}

type scrubber struct {
	maps   map[string]map[string]string // kind -> lowercased original -> placeholder
	viewer string
}

// name returns a stable placeholder such as "user-3" for an original value.
func (s *scrubber) name(kind, orig string) string {
	if orig == "" {
		return ""
	}
	m := s.maps[kind]
	if m == nil {
		m = map[string]string{}
		s.maps[kind] = m
	}
	k := strings.ToLower(orig)
	if _, ok := m[k]; !ok {
		m[k] = fmt.Sprintf("%s-%d", kind, len(m)+1)
	}
	return m[k]
}

func (s *scrubber) org(login string) string { return s.name("org", login) }

func (s *scrubber) team(org, slug string) string {
	return s.org(org) + "/" + s.name("team", org+"/"+slug)
}

func (s *scrubber) login(typename, login string) string {
	if EqualFold(login, s.viewer) {
		return "me"
	}
	for _, b := range publicBots {
		if strings.HasPrefix(strings.ToLower(login), b) {
			return login
		}
	}
	if typename == "Bot" {
		return s.name("bot", login)
	}
	return s.name("user", login)
}

func (s *scrubber) actor(a *Actor) { a.Login = s.login(a.Typename, a.Login) }

func (s *scrubber) reviewer(r *Reviewer) {
	if !r.IsTeam() {
		r.Login = s.login(r.Typename, r.Login)
		return
	}
	org := ""
	if r.Organization != nil {
		org = r.Organization.Login
		r.Organization.Login = s.org(org)
	}
	r.Slug = s.name("team", org+"/"+r.Slug)
}
