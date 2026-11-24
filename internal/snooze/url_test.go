package snooze

import "testing"

func TestCanonical(t *testing.T) {
	const canon = "https://github.com/Org-1/Repo-1/pull/106"
	for _, tc := range []struct {
		in   string
		want string // "" means not a PR reference
	}{
		{canon, canon},
		{canon + "/", canon},
		{canon + "/files", canon},
		{canon + "/commits/abc?x=1#y", canon},
		{canon + "?diff=split", canon},
		{canon + "#discussion_r1", canon},
		{"http://github.com/Org-1/Repo-1/pull/106", canon},
		{"HTTPS://GitHub.com/Org-1/Repo-1/PULL/106", canon},
		{"  " + canon + " ", canon},
		{"Org-1/Repo-1#106", canon},
		{"https://ghe.example.test/org-1/repo-1/pull/7", "https://ghe.example.test/org-1/repo-1/pull/7"},
		{"", ""},
		{"github.com/org-1/repo-1/pull/1", ""}, // no scheme
		{"https://github.com/org-1/repo-1", ""},
		{"https://github.com/org-1/repo-1/issues/1", ""},
		{"https://github.com/org-1/repo-1/pull/abc", ""},
		{"https://github.com/org-1/repo-1/pull/0", ""},
		{"ftp://github.com/org-1/repo-1/pull/1", ""},
		{"org-1/repo-1#0", ""},
		{"org-1#1", ""},
		{"testdata/case.json", ""},
	} {
		got, ok := Canonical(tc.in)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("Canonical(%q) = (%q, %v), want %q", tc.in, got, ok, tc.want)
		}
	}
}

func TestKey(t *testing.T) {
	if got := Key("HTTP://github.com/Org-1/Repo-1/pull/106/files?x#y"); got != "https://github.com/org-1/repo-1/pull/106" {
		t.Errorf("Key = %q", got)
	}
	if got := Key("  Not A PR  "); got != "not a pr" { // unrecognised: lowercased text
		t.Errorf("Key of text = %q", got)
	}
}

func TestIsShortRef(t *testing.T) {
	for in, want := range map[string]bool{"org-1/repo-1#106": true, " org-1/repo-1#1 ": true, "org-1/repo-1": false, "org-1/repo-1#": false, "a/b/c#1": false, "https://github.com/org-1/repo-1/pull/1": false} {
		if got := IsShortRef(in); got != want {
			t.Errorf("IsShortRef(%q) = %v", in, got)
		}
	}
}
