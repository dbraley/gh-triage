package fetch

import (
	"regexp"
	"strings"
	"testing"
)

// block returns the text of the {...} that follows the first occurrence of
// prefix in q.
func block(t *testing.T, q, prefix string) string {
	t.Helper()
	i := strings.Index(q, prefix)
	if i < 0 {
		t.Fatalf("query has no %q", prefix)
	}
	j := strings.Index(q[i:], "{")
	if j < 0 {
		t.Fatalf("no selection after %q", prefix)
	}
	depth, start := 0, i+j
	for k := start; k < len(q); k++ {
		switch q[k] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return q[start : k+1]
			}
		}
	}
	t.Fatalf("unbalanced braces after %q", prefix)
	return ""
}

// Snooze wakes on conversation comments and reviews by someone else, which
// needs their timestamps; no free text may be requested (it is private).
func TestPRQueryRequestsWakeFields(t *testing.T) {
	if got := block(t, prQuery, "comments(last:"); !strings.Contains(got, "createdAt") {
		t.Errorf("comments(last:...) does not request createdAt: %s", got)
	}
	if got := block(t, prQuery, "reviews(last:"); !strings.Contains(got, "submittedAt") {
		t.Errorf("reviews(last:...) does not request submittedAt: %s", got)
	}
	if re := regexp.MustCompile(`\bbody\b`); re.MatchString(prQuery) {
		t.Error("the PR query must not request any body field")
	}
}
