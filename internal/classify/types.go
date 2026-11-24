// Package classify decides which bucket a PR belongs in. Everything here is
// a pure function of a fetch.PR plus Options: no network, no clock.
package classify

import (
	"regexp"
	"strings"
	"time"
)

type Bucket string

const (
	// PRs I authored.
	ActNow         Bucket = "ACT NOW"
	InMergeQueue   Bucket = "IN MERGE QUEUE"
	ReadyToEnqueue Bucket = "READY TO ENQUEUE"
	Poke           Bucket = "POKE"
	WaitingOnCI    Bucket = "WAITING ON CI"
	Draft          Bucket = "DRAFT"
	// PRs by others.
	ReviewRequested Bucket = "REVIEW REQUESTED"
	ReReview        Bucket = "RE-REVIEW"
	ReplyOwed       Bucket = "REPLY OWED"
	TeamOnly        Bucket = "TEAM REVIEW"
	// Involved, but nothing is owed by me right now.
	Nothing Bucket = "NOTHING TO DO"
)

// AuthoredOrder and ReviewOrder are display order (highest priority first).
var (
	AuthoredOrder = []Bucket{ActNow, ReadyToEnqueue, Poke, InMergeQueue, WaitingOnCI, Draft}
	ReviewOrder   = []Bucket{ReviewRequested, ReReview, ReplyOwed, TeamOnly}
)

type BotPattern struct {
	Pattern string `toml:"pattern"` // glob on the author login, "*" only
	Label   string `toml:"label"`
}

// DefaultBots are checked after any configured patterns. Authors GitHub
// reports as __typename Bot are always bots, matched or not.
var DefaultBots = []BotPattern{
	{"dependabot*", "Dependabot"},
	{"renovate*", "Renovate"},
	{"github-actions*", "GitHub Actions"},
	{"*[bot]", ""},
}

type Options struct {
	Me    string    // viewer login
	Teams []string  // "org/slug" the viewer belongs to
	Now   time.Time // weekends are judged in Now's location

	TeamRequestsAreMine bool          // team-only request counts as REVIEW REQUESTED
	PokeAfter           time.Duration // business time; 24h = 1 business day
	Bots                []BotPattern  // checked before DefaultBots
	// When a PR reports no required checks at all (no branch protection),
	// treat every check as required. Otherwise failures would be invisible.
	AllChecksWhenNoneRequired bool
}

// Holder is someone (or a team) holding the ball on a PR.
type Holder struct {
	Who      string // login, or "org/slug" for a team
	IsTeam   bool
	ReReview bool          // they reviewed an earlier commit
	Since    time.Time     // later of last push / their review request
	Idle     time.Duration // business time from Since to Now
}

type Result struct {
	Bucket Bucket
	Reason string
	Trail  []string // every rule evaluated, in order, for --explain

	Mine     bool // authored by the viewer
	Bot      bool // authored by a bot
	BotLabel string

	Holders  []Holder  // who to poke; set when review is the only thing missing
	SortTime time.Time // oldest first within a bucket
}

// PokeEntry is one line of the poke list.
type PokeEntry struct {
	Holder Holder
	Ref    string
	Title  string
	URL    string
	Bot    bool
}

// Glob matches s against a pattern where "*" is the only wildcard,
// case-insensitively. (path.Match would treat "[bot]" as a character class.)
func Glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("(?i)^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(s)
}

// BusinessDuration is the time between from and to, not counting Saturdays
// and Sundays (in to's location).
func BusinessDuration(from, to time.Time) time.Duration {
	if !to.After(from) {
		return 0
	}
	cur := from.In(to.Location())
	var total time.Duration
	for cur.Before(to) {
		next := time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, cur.Location())
		if next.After(to) {
			next = to
		}
		if wd := cur.Weekday(); wd != time.Saturday && wd != time.Sunday {
			total += next.Sub(cur)
		}
		cur = next
	}
	return total
}

// FormatIdle renders a business duration as "<1h", "5h", "2d", "2d 5h".
func FormatIdle(d time.Duration) string {
	h := int(d.Hours())
	switch {
	case h < 1:
		return "<1h"
	case h < 24:
		return itoa(h) + "h"
	case h%24 == 0:
		return itoa(h/24) + "d"
	}
	return itoa(h/24) + "d " + itoa(h%24) + "h"
}
