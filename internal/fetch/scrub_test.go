package fetch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestScrubRemovesPrivateNames(t *testing.T) {
	raw := `{
	  "fetchedAt": "2026-11-18T17:00:00Z", "viewer": "RealMe", "viewerTeams": ["SecretOrg/platform"],
	  "node": {
	    "id": "PR_kwDOsecret", "number": 4242, "title": "Rotate the secret-sauce keys",
	    "url": "https://github.com/SecretOrg/hush/pull/4242", "bodyText": "never fetched, never kept",
	    "repository": {"nameWithOwner": "SecretOrg/hush", "owner": {"login": "SecretOrg"}},
	    "author": {"__typename": "User", "login": "realme"},
	    "commits": {"nodes": [{"commit": {"oid": "deadbeefcafe", "statusCheckRollup": {"contexts": {"nodes": [
	      {"__typename": "CheckRun", "name": "hush-build", "status": "COMPLETED", "conclusion": "SUCCESS", "isRequired": true}]}}}}]},
	    "latestReviews": {"nodes": [{"author": {"login": "alice"}, "state": "COMMENTED", "commit": {"oid": "deadbeefcafe"}}]},
	    "comments": {"nodes": [
	      {"author": {"__typename": "User", "login": "RealMe"}, "createdAt": "2026-11-17T10:00:00Z"},
	      {"author": {"__typename": "User", "login": "zed"}, "createdAt": "2026-11-17T11:00:00Z"},
	      {"author": {"__typename": "Bot", "login": "github-actions"}, "createdAt": "2026-11-17T12:00:00Z"}]},
	    "reviews": {"nodes": [
	      {"author": {"__typename": "User", "login": "Zed"}, "state": "APPROVED", "submittedAt": "2026-11-17T13:00:00Z", "commit": {"oid": "feedfacebabe"}},
	      {"author": {"__typename": "User", "login": "alice"}, "state": "COMMENTED", "submittedAt": "2026-11-17T14:00:00Z", "commit": {"oid": "deadbeefcafe"}}]},
	    "reviewRequests": {"nodes": [
	      {"requestedReviewer": {"__typename": "Team", "slug": "platform", "organization": {"login": "SecretOrg"}}},
	      {"requestedReviewer": {"__typename": "User", "login": "alice"}},
	      {"requestedReviewer": {"__typename": "Bot", "login": "dependabot"}}]},
	    "reviewThreads": {"nodes": [{"comments": {"nodes": [{"author": {"login": "Alice"}}]}}]},
	    "timelineItems": {"nodes": [{"__typename": "ReviewRequestedEvent", "requestedReviewer": {"__typename": "User", "login": "alice"}}]}
	  }
	}`
	var f Fixture
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	s := Scrub(f)
	out, _ := json.Marshal(s)
	for _, private := range []string{"realme", "secretorg", "hush", "platform", "alice", "secret-sauce", "4242", "deadbeef", "feedface", "zed", "never"} {
		if strings.Contains(strings.ToLower(string(out)), private) {
			t.Errorf("scrubbed output still contains %q:\n%s", private, out)
		}
	}

	// What classification depends on survives, consistently renamed.
	n := s.Node
	if s.Viewer != "me" || n.Author.Login != "me" {
		t.Errorf("viewer/author = %q/%q", s.Viewer, n.Author.Login)
	}
	if got := n.ReviewRequests.Nodes[0].RequestedReviewer.Name(); len(s.ViewerTeams) != 1 || got != s.ViewerTeams[0] {
		t.Errorf("team request %q does not match viewer teams %v", got, s.ViewerTeams)
	}
	alice := n.ReviewRequests.Nodes[1].RequestedReviewer.Login
	if alice != n.LatestReviews.Nodes[0].Author.Login || alice != n.ReviewThreads.Nodes[0].Comments.Nodes[0].Author.Login {
		t.Error("one login mapped to different placeholders")
	}
	if n.ReviewRequests.Nodes[2].RequestedReviewer.Login != "dependabot" {
		t.Error("public bot login should be kept")
	}
	if cs := n.Comments.Nodes; cs[0].Author.Login != "me" || cs[1].Author.Login == "zed" || cs[2].Author.Login != "github-actions" {
		t.Errorf("comment authors = %q, %q, %q", cs[0].Author.Login, cs[1].Author.Login, cs[2].Author.Login)
	}
	rv := n.Reviews.Nodes
	if rv[0].Author.Login != n.Comments.Nodes[1].Author.Login {
		t.Error("one login mapped to different placeholders across comments and reviews")
	}
	if rv[1].Author.Login != alice || rv[1].CommitOID() != n.Head().OID || rv[0].CommitOID() == n.Head().OID {
		t.Errorf("review authors/oids not consistently scrubbed: %+v", rv)
	}
	if n.Head().OID != n.LatestReviews.Nodes[0].CommitOID() {
		t.Error("commit ids no longer line up")
	}
	if ch := n.Head().StatusCheckRollup.Contexts.Nodes[0]; !ch.IsRequired || ch.Conclusion != "SUCCESS" {
		t.Errorf("check state lost: %+v", ch)
	}
	if !s.FetchedAt.Equal(time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("fetchedAt = %v", s.FetchedAt)
	}
}
