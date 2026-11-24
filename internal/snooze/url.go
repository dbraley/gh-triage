package snooze

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var shortRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#([0-9]+)$`)

// Canonical turns a reference to a PR into its canonical URL,
// https://host/owner/repo/pull/N. It accepts a PR URL with any scheme, a
// trailing slash, a path after the number (/files), a query or a fragment,
// and the short form owner/repo#N (github.com is assumed). The case of the
// owner and repo is kept; only the host is lowercased. ok is false when ref
// is not a PR reference.
func Canonical(ref string) (canon string, ok bool) {
	ref = strings.TrimSpace(ref)
	if m := shortRE.FindStringSubmatch(ref); m != nil {
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			return "", false
		}
		return "https://github.com/" + m[1] + "/" + m[2] + "/pull/" + strconv.Itoa(n), true
	}
	if !strings.Contains(ref, "://") {
		return "", false
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 4 || segs[0] == "" || segs[1] == "" || !strings.EqualFold(segs[2], "pull") {
		return "", false
	}
	n, err := strconv.Atoi(segs[3])
	if err != nil || n <= 0 {
		return "", false
	}
	return "https://" + strings.ToLower(u.Hostname()) + "/" + segs[0] + "/" + segs[1] + "/pull/" + strconv.Itoa(n), true
}

// Key is the store key for a PR reference: the lowercased canonical URL, or,
// for anything that is not a PR reference, the lowercased trimmed text.
func Key(ref string) string {
	if c, ok := Canonical(ref); ok {
		return strings.ToLower(c)
	}
	return strings.ToLower(strings.TrimSpace(ref))
}

// IsShortRef reports whether ref has the form owner/repo#N.
func IsShortRef(ref string) bool { return shortRE.MatchString(strings.TrimSpace(ref)) }
