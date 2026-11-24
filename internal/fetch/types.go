// Package fetch holds the GraphQL query, the types its response decodes
// into, and the (only) code that talks to GitHub.
package fetch

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// Fixture is one dumped PR plus the context needed to classify it offline.
// `--dump` writes these; tests and `--from` read them.
type Fixture struct {
	FetchedAt   time.Time `json:"fetchedAt"`
	Viewer      string    `json:"viewer"`
	ViewerTeams []string  `json:"viewerTeams"` // "org/slug"
	Node        PR        `json:"node"`
}

// LoadFixture reads a fixture file. No network.
func LoadFixture(path string) (Fixture, error) {
	var f Fixture
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

type Actor struct {
	Typename string `json:"__typename,omitempty"`
	Login    string `json:"login"`
}

// Reviewer is a requested reviewer: a User/Bot/Mannequin (Login) or a Team
// (Slug + Organization).
type Reviewer struct {
	Typename     string `json:"__typename"`
	Login        string `json:"login,omitempty"`
	Slug         string `json:"slug,omitempty"`
	Organization *Actor `json:"organization,omitempty"`
}

func (r Reviewer) IsTeam() bool { return r.Typename == "Team" }

// Name is the login for people and "org/slug" for teams.
func (r Reviewer) Name() string {
	if r.IsTeam() {
		if r.Organization != nil {
			return r.Organization.Login + "/" + r.Slug
		}
		return r.Slug
	}
	return r.Login
}

// Check is a CheckRun or a StatusContext on the head commit.
type Check struct {
	Typename    string     `json:"__typename"`
	Name        string     `json:"name,omitempty"`       // CheckRun
	Status      string     `json:"status,omitempty"`     // CheckRun
	Conclusion  string     `json:"conclusion,omitempty"` // CheckRun
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Context     string     `json:"context,omitempty"` // StatusContext
	State       string     `json:"state,omitempty"`   // StatusContext
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	IsRequired  bool       `json:"isRequired"`
}

func (c Check) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// When is the best available timestamp for ordering reruns of one check.
func (c Check) When() time.Time {
	for _, t := range []*time.Time{c.StartedAt, c.CreatedAt, c.CompletedAt} {
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

type Commit struct {
	OID               string    `json:"oid"`
	CommittedDate     time.Time `json:"committedDate"`
	StatusCheckRollup *struct {
		State    string `json:"state"`
		Contexts struct {
			TotalCount int     `json:"totalCount"`
			Nodes      []Check `json:"nodes"`
		} `json:"contexts"`
	} `json:"statusCheckRollup"`
}

type Review struct {
	Author      Actor     `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Commit      *struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

func (r Review) CommitOID() string {
	if r.Commit == nil {
		return ""
	}
	return r.Commit.OID
}

type Comment struct {
	Author    Actor     `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
}

type Thread struct {
	IsResolved bool `json:"isResolved"`
	IsOutdated bool `json:"isOutdated"`
	Comments   struct {
		Nodes []Comment `json:"nodes"`
	} `json:"comments"`
}

// TimelineItem is one of the event types the query asks for; which fields
// are set depends on Typename.
type TimelineItem struct {
	Typename          string    `json:"__typename"`
	CreatedAt         time.Time `json:"createdAt"`
	RequestedReviewer *Reviewer `json:"requestedReviewer,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Actor             *Actor    `json:"actor,omitempty"`
}

type MergeQueueEntry struct {
	State                string     `json:"state"`
	Position             int        `json:"position"`
	EstimatedTimeToMerge *int       `json:"estimatedTimeToMerge"` // seconds
	EnqueuedAt           *time.Time `json:"enqueuedAt"`
}

type PR struct {
	ID         string    `json:"id"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	IsDraft    bool      `json:"isDraft"`
	CreatedAt  time.Time `json:"createdAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
		IsArchived    bool   `json:"isArchived"`
		Owner         Actor  `json:"owner"`
	} `json:"repository"`
	Author              Actor            `json:"author"`
	ReviewDecision      string           `json:"reviewDecision"`
	Mergeable           string           `json:"mergeable"`
	MergeStateStatus    string           `json:"mergeStateStatus"`
	IsInMergeQueue      bool             `json:"isInMergeQueue"`
	IsMergeQueueEnabled bool             `json:"isMergeQueueEnabled"`
	MergeQueueEntry     *MergeQueueEntry `json:"mergeQueueEntry"`
	AutoMergeRequest    *struct {
		EnabledAt *time.Time `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	Commits struct {
		Nodes []struct {
			Commit Commit `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	LatestOpinionatedReviews struct {
		Nodes []Review `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	LatestReviews struct {
		Nodes []Review `json:"nodes"`
	} `json:"latestReviews"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer Reviewer `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	ReviewThreads struct {
		Nodes []Thread `json:"nodes"`
	} `json:"reviewThreads"`
	TimelineItems struct {
		Nodes []TimelineItem `json:"nodes"`
	} `json:"timelineItems"`
}

// Head is the PR's head commit (zero value if the response had none).
func (p PR) Head() Commit {
	if n := len(p.Commits.Nodes); n > 0 {
		return p.Commits.Nodes[n-1].Commit
	}
	return Commit{}
}

// Ref is "owner/repo#123".
func (p PR) Ref() string {
	return p.Repository.NameWithOwner + "#" + itoa(p.Number)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// EqualFold compares GitHub logins/slugs, which are case-insensitive.
func EqualFold(a, b string) bool { return a != "" && strings.EqualFold(a, b) }
