package fetch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Client talks to GitHub's GraphQL API using the gh CLI's stored login.
type Client struct{ gql *api.GraphQLClient }

func NewClient() (*Client, error) {
	gql, err := api.DefaultGraphQLClient()
	if err != nil {
		return nil, fmt.Errorf("no GitHub login found (run `gh auth login`): %w", err)
	}
	return &Client{gql}, nil
}

// Ref identifies a PR found by search, before its details are fetched.
type Ref struct {
	ID         string `json:"id"`
	Number     int    `json:"number"`
	URL        string `json:"url"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
		IsArchived    bool   `json:"isArchived"`
		Owner         Actor  `json:"owner"`
	} `json:"repository"`
}

func (c *Client) Viewer() (string, error) {
	var resp struct{ Viewer Actor }
	err := c.gql.Do(viewerQuery, nil, &resp)
	return resp.Viewer.Login, err
}

// Teams lists the "org/slug" teams the viewer is a member of. It needs the
// read:org scope and SSO authorisation for each org.
func (c *Client) Teams(login string) ([]string, error) {
	var teams []string
	vars := map[string]any{"login": login}
	for {
		var resp struct {
			Viewer struct {
				Organizations struct {
					PageInfo pageInfo
					Nodes    []struct {
						Login string
						Teams struct{ Nodes []struct{ Slug string } }
					}
				}
			}
		}
		// Partial data is still useful when one org blocks the token.
		err := c.gql.Do(teamsQuery, vars, &resp)
		orgs := resp.Viewer.Organizations
		for _, org := range orgs.Nodes {
			for _, t := range org.Teams.Nodes {
				teams = append(teams, org.Login+"/"+t.Slug)
			}
		}
		if err != nil {
			return teams, err
		}
		if !orgs.PageInfo.HasNextPage {
			return teams, nil
		}
		vars["after"] = orgs.PageInfo.EndCursor
	}
}

type pageInfo struct {
	HasNextPage bool
	EndCursor   string
}

// Search returns every PR matching a GitHub search query.
func (c *Client) Search(q string) ([]Ref, error) {
	var refs []Ref
	vars := map[string]any{"q": q}
	for {
		var resp struct {
			Search struct {
				PageInfo pageInfo
				Nodes    []Ref
			}
		}
		if err := c.gql.Do(searchQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("search %q: %w", q, err)
		}
		for _, r := range resp.Search.Nodes {
			if r.ID != "" { // non-PR results decode to empty
				refs = append(refs, r)
			}
		}
		if !resp.Search.PageInfo.HasNextPage {
			return refs, nil
		}
		vars["after"] = resp.Search.PageInfo.EndCursor
	}
}

// Resolve turns a PR URL into a Ref.
func (c *Client) Resolve(url string) (Ref, error) {
	var resp struct{ Resource Ref }
	if err := c.gql.Do(resolveQuery, map[string]any{"url": url}, &resp); err != nil {
		return Ref{}, err
	}
	if resp.Resource.ID == "" {
		return Ref{}, fmt.Errorf("%s is not a pull request you can see", url)
	}
	return resp.Resource, nil
}

// Fetched is a PR with the raw GraphQL node it was decoded from.
type Fetched struct {
	PR  PR
	Raw json.RawMessage
}

func (c *Client) PR(id string) (Fetched, error) {
	var resp struct {
		Node json.RawMessage `json:"node"`
	}
	if err := c.gql.Do(prQuery, map[string]any{"id": id}, &resp); err != nil {
		return Fetched{}, err
	}
	f := Fetched{Raw: resp.Node}
	return f, json.Unmarshal(resp.Node, &f.PR)
}

// PRs fetches details for all refs with a few requests in flight. PRs that
// fail are skipped and reported in errs.
func (c *Client) PRs(refs []Ref) (out []Fetched, errs []error) {
	results := make([]Fetched, len(refs))
	failures := make([]error, len(refs))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, r := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], failures[i] = c.PR(r.ID)
		}()
	}
	wg.Wait()
	for i := range refs {
		if failures[i] != nil {
			errs = append(errs, fmt.Errorf("%s: %w", refs[i].URL, failures[i]))
			continue
		}
		out = append(out, results[i])
	}
	return out, errs
}

// Dump writes a fixture holding the raw response to dir and returns its path.
// These contain private data: keep them in testdata/local (gitignored) and
// run them through Scrub before committing.
func Dump(dir, viewer string, teams []string, at time.Time, f Fetched) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	doc := struct {
		FetchedAt   time.Time       `json:"fetchedAt"`
		Viewer      string          `json:"viewer"`
		ViewerTeams []string        `json:"viewerTeams"`
		Node        json.RawMessage `json:"node"`
	}{at.UTC().Truncate(time.Second), viewer, teams, f.Raw}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, b, "", "  "); err != nil {
		return "", err
	}
	pretty.WriteByte('\n')
	name := strings.NewReplacer("/", "_", "#", "_").Replace(f.PR.Ref()) + ".json"
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, pretty.Bytes(), 0o644)
}
