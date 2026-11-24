package snooze

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dbraley/gh-triage/internal/classify"
)

func entry(url string) Entry {
	return Entry{
		URL:       url,
		SnoozedAt: time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC),
		Until:     time.Date(2025, 1, 9, 9, 0, 0, 0, time.UTC),
		HeadOID:   "aaa",
		Bucket:    classify.Poke,
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "snoozes.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, a := entry("https://github.com/org-1/repo-1/pull/2"), entry("https://github.com/org-1/repo-1/pull/1")
	s.Put(b)
	s.Put(a)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{a, b}; !reflect.DeepEqual(got.Entries(), want) {
		t.Errorf("Entries = %+v, want %+v", got.Entries(), want)
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.Entries()); n != 0 {
		t.Errorf("got %d entries, want 0", n)
	}
}

func TestStoreBlankFileIsEmpty(t *testing.T) {
	for _, content := range []string{"", "  \n\t\n"} {
		path := filepath.Join(t.TempDir(), "s.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Load(path)
		if err != nil || len(s.Entries()) != 0 {
			t.Errorf("Load(%q) = %v entries, %v; want empty store", content, s, err)
		}
	}
}

func TestStoreCorruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("want error for corrupt file")
	}
}

func TestStoreCaseInsensitiveKey(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "s.json"))
	s.Put(entry("https://github.com/Org-1/Repo-1/pull/1"))
	if _, ok := s.Get("https://github.com/org-1/repo-1/PULL/1"); !ok {
		t.Error("Get should ignore case")
	}
	newer := entry("https://github.com/org-1/repo-1/pull/1")
	newer.Until = newer.Until.Add(48 * time.Hour)
	s.Put(newer) // replaces, not duplicates
	if n := len(s.Entries()); n != 1 {
		t.Errorf("got %d entries, want 1", n)
	}
	if got, _ := s.Get(newer.URL); !got.Until.Equal(newer.Until) {
		t.Errorf("Until = %v, want the newer entry's %v", got.Until, newer.Until)
	}
}

func TestStoreNormalisesKeys(t *testing.T) {
	const canon = "https://github.com/org-1/repo-1/pull/106"
	forms := []string{
		canon, canon + "/", canon + "/files", canon + "/files/abc123", canon + "?diff=split",
		canon + "#issuecomment-1", "http://github.com/org-1/repo-1/pull/106", "HTTPS://GITHUB.COM/ORG-1/REPO-1/PULL/106",
		"org-1/repo-1#106", "  " + canon + "\n",
	}
	for _, f := range forms {
		s, _ := Load(filepath.Join(t.TempDir(), "s.json"))
		s.Put(entry(canon))
		if _, ok := s.Get(f); !ok {
			t.Errorf("Get(%q) missed", f)
		}
		if !s.Delete(f) || len(s.Entries()) != 0 {
			t.Errorf("Delete(%q) did not remove the entry", f)
		}
		// And the other way round: a form stored, the canonical URL looked up.
		s.Put(entry(f))
		if _, ok := s.Get(canon); !ok {
			t.Errorf("entry stored as %q not found by the canonical URL", f)
		}
		s.Put(entry(canon))
		if n := len(s.Entries()); n != 1 {
			t.Errorf("%q and the canonical URL made %d entries", f, n)
		}
	}
	s, _ := Load(filepath.Join(t.TempDir(), "s.json"))
	s.Put(entry(canon))
	for _, other := range []string{"https://github.com/org-1/repo-1/pull/16", "https://github.com/org-1/repo-1/pull/1060", "org-1/repo-2#106", "https://github.com/org-1/repo-1/issues/106"} {
		if _, ok := s.Get(other); ok {
			t.Errorf("Get(%q) matched a different PR", other)
		}
	}
}

func TestStoreDelete(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "s.json"))
	s.Put(entry("https://github.com/org-1/repo-1/pull/1"))
	if !s.Delete("HTTPS://github.com/org-1/repo-1/pull/1") {
		t.Error("Delete of existing entry = false")
	}
	if s.Delete("https://github.com/org-1/repo-1/pull/1") {
		t.Error("second Delete = true")
	}
	if _, ok := s.Get("https://github.com/org-1/repo-1/pull/1"); ok {
		t.Error("entry still present")
	}
}

func TestStoreFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "snoozes.json")
	s, _ := Load(path)
	s.Put(entry("https://github.com/org-1/repo-1/pull/1"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil { // overwrite path
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("file mode = %o, want 600", m)
	}
	di, _ := os.Stat(dir)
	if m := di.Mode().Perm(); m != 0o700 {
		t.Errorf("dir mode = %o, want 700", m)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover temp files: %v", ents)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got, err := DefaultPath(); err != nil || got != "/xdg/state/gh-triage/snoozes.json" {
		t.Errorf("DefaultPath = %q, %v", got, err)
	}
	t.Setenv("HOME", "/home/user-1")
	// Unset, empty and relative values fall back to ~/.local/state (XDG spec).
	for _, v := range []string{"", "relative/state", "./state"} {
		t.Setenv("XDG_STATE_HOME", v)
		if got, err := DefaultPath(); err != nil || got != "/home/user-1/.local/state/gh-triage/snoozes.json" {
			t.Errorf("XDG_STATE_HOME=%q: DefaultPath = %q, %v", v, got, err)
		}
	}
}
