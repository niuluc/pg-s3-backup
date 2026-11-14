package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Target is one database we back up.
type Target struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Database      string `json:"database"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Schedule      string `json:"schedule"`
	RetentionDays int    `json:"retentionDays"`
	Prefix        string `json:"prefix"`
	Enabled       bool   `json:"enabled"`

	// Last detected server version, e.g. "16.4". Empty until first contact.
	ServerVersion string    `json:"serverVersion"`
	DetectedAt    time.Time `json:"detectedAt"`
}

// Run is one backup or verify job.
type Run struct {
	ID            string     `json:"id"`
	TargetID      string     `json:"targetId"`
	Kind          string     `json:"kind"`    // backup | verify
	Trigger       string     `json:"trigger"` // schedule | <user>
	Status        string     `json:"status"`  // running | ok | failed
	Started       time.Time  `json:"started"`
	Finished      *time.Time `json:"finished,omitempty"`
	File          string     `json:"file,omitempty"`
	Size          int64      `json:"size,omitempty"`
	ServerVersion string     `json:"serverVersion,omitempty"`
	Client        string     `json:"client,omitempty"`
	Message       string     `json:"message,omitempty"`
	Log           string     `json:"log"`
}

type state struct {
	Targets []*Target `json:"targets"`
	Runs    []*Run    `json:"runs"`
}

// Store keeps all state in one JSON file. Fine for a handful of targets.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
}

const runsPerTarget = 30

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, err
	}
	// A restart kills running jobs; mark them so the UI does not spin forever.
	for _, r := range s.st.Runs {
		if r.Status == "running" {
			r.Status = "failed"
			r.Message = "interrupted by restart"
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) Targets() []Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Target, 0, len(s.st.Targets))
	for _, t := range s.st.Targets {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Target(id string) (Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.st.Targets {
		if t.ID == id {
			return *t, true
		}
	}
	return Target{}, false
}

func (s *Store) PutTarget(t Target) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = newID()
		s.st.Targets = append(s.st.Targets, &t)
		return t, s.saveLocked()
	}
	for i, old := range s.st.Targets {
		if old.ID == t.ID {
			s.st.Targets[i] = &t
			return t, s.saveLocked()
		}
	}
	return t, errors.New("target not found")
}

func (s *Store) SetDetected(id, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.st.Targets {
		if t.ID == id {
			t.ServerVersion = version
			t.DetectedAt = time.Now().UTC()
		}
	}
	_ = s.saveLocked()
}

func (s *Store) DeleteTarget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.st.Targets[:0]
	for _, t := range s.st.Targets {
		if t.ID != id {
			kept = append(kept, t)
		}
	}
	s.st.Targets = kept
	runs := s.st.Runs[:0]
	for _, r := range s.st.Runs {
		if r.TargetID != id {
			runs = append(runs, r)
		}
	}
	s.st.Runs = runs
	return s.saveLocked()
}

func (s *Store) AddRun(r *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Runs = append(s.st.Runs, r)
	// Trim history per target.
	count := map[string]int{}
	keep := make([]*Run, 0, len(s.st.Runs))
	for i := len(s.st.Runs) - 1; i >= 0; i-- {
		run := s.st.Runs[i]
		count[run.TargetID]++
		if count[run.TargetID] <= runsPerTarget {
			keep = append(keep, run)
		}
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	s.st.Runs = keep
	_ = s.saveLocked()
}

// UpdateRun applies fn to the stored run under the lock and saves.
func (s *Store) UpdateRun(id string, fn func(r *Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.st.Runs {
		if r.ID == id {
			fn(r)
		}
	}
	_ = s.saveLocked()
}

func (s *Store) Run(id string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.st.Runs {
		if r.ID == id {
			return *r, true
		}
	}
	return Run{}, false
}

// Runs returns runs for a target, newest first.
func (s *Store) Runs(targetID string) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for i := len(s.st.Runs) - 1; i >= 0; i-- {
		if r := s.st.Runs[i]; r.TargetID == targetID {
			out = append(out, *r)
		}
	}
	return out
}

// LastRun returns the newest run of a kind for a target.
func (s *Store) LastRun(targetID, kind string) (Run, bool) {
	for _, r := range s.Runs(targetID) {
		if r.Kind == kind {
			return r, true
		}
	}
	return Run{}, false
}

// LastFinished returns the newest finished run of a kind.
func (s *Store) LastFinished(targetID, kind string) (Run, bool) {
	for _, r := range s.Runs(targetID) {
		if r.Kind == kind && r.Status != "running" {
			return r, true
		}
	}
	return Run{}, false
}
