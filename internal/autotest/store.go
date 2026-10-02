package autotest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// Store keeps tests and runs as JSON files:
//
//	<dir>/tests/<id>.json
//	<dir>/runs/<id>.json
//	<dir>/runs/<id>/<n>.jpg   step screenshots
type Store struct {
	dir string
	mu  sync.Mutex
}

func OpenStore(dir string) (*Store, error) {
	for _, d := range []string{"tests", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var validID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Store) path(kind, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", protocol.Errorf(protocol.CodeNotFound, "%s %q not found", kind[:len(kind)-1], id)
	}
	return filepath.Join(s.dir, kind, id+".json"), nil
}

func (s *Store) write(kind, id string, v any) error {
	p, err := s.path(kind, id)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) read(kind, id string, v any) error {
	p, err := s.path(kind, id)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Errorf(protocol.CodeNotFound, "%s %q not found", kind[:len(kind)-1], id)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func list[T any](s *Store, kind string) ([]T, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, kind, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v T
		if json.Unmarshal(b, &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *Store) SaveTest(t *Test) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write("tests", t.ID, t)
}

func (s *Store) GetTest(id string) (*Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Test
	return &t, s.read("tests", id, &t)
}

func (s *Store) DeleteTest(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path("tests", id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Tests returns all tests, most recently updated first.
func (s *Store) Tests() ([]Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := list[Test](s, "tests")
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, err
}

func (s *Store) SaveRun(r *Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write("runs", r.ID, r)
}

func (s *Store) GetRun(id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Run
	return &r, s.read("runs", id, &r)
}

// Runs returns runs (optionally for one test), newest first, without logs.
func (s *Store) Runs(testID string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := list[Run](s, "runs")
	out := all[:0]
	for _, r := range all {
		if testID == "" || r.TestID == testID {
			r.Log = nil
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, err
}

// RunFile returns the path of a file in a run's directory.
func (s *Store) RunFile(runID, name string) (string, error) {
	if _, err := s.path("runs", runID); err != nil {
		return "", err
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return "", protocol.Errorf(protocol.CodeBadRequest, "bad file name")
	}
	dir := filepath.Join(s.dir, "runs", runID)
	return filepath.Join(dir, name), os.MkdirAll(dir, 0o700)
}
