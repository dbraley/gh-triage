package snooze

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store is the snooze state file: a JSON array of entries.
type Store struct {
	path    string
	entries map[string]Entry // keyed by Key(URL)
}

// DefaultPath is $XDG_STATE_HOME/gh-triage/snoozes.json, falling back to
// ~/.local/state/gh-triage/snoozes.json. A relative XDG_STATE_HOME is ignored,
// as the XDG spec requires.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "gh-triage", "snoozes.json"), nil
}

func key(ref string) string { return Key(ref) }

// Load reads the store. A missing file is an empty store.
func Load(path string) (*Store, error) {
	s := &Store{path: path, entries: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	var list []Entry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, e := range list {
		s.Put(e)
	}
	return s, nil
}

func (s *Store) Get(url string) (Entry, bool) {
	e, ok := s.entries[key(url)]
	return e, ok
}

func (s *Store) Put(e Entry) { s.entries[key(e.URL)] = e }

// Delete reports whether the entry existed.
func (s *Store) Delete(url string) bool {
	_, ok := s.entries[key(url)]
	delete(s.entries, key(url))
	return ok
}

// Entries are sorted by URL so the file and any listing are deterministic.
func (s *Store) Entries() []Entry {
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i].URL) < key(out[j].URL) })
	return out
}

// Save writes the file atomically (temp file + rename) so a crash never
// leaves a truncated store.
func (s *Store) Save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.Entries(), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snoozes-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
