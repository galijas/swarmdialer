// Package logstore writes and manages one plain-text log file per
// completed call batch (see cmd/gui/handlers.go's batch-completion
// handling), kept separately for local and remote dialing — the "Log
// browser" feature in each dialer panel, and Settings' "clear all logs"
// (including as part of resetting SwarmDialer), read/list/delete through
// this package rather than touching files directly.
package logstore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Section is which dialer produced a batch — matches the dashboard's
// "local"/"remote" terminology throughout the rest of the app.
type Section string

const (
	SectionLocal  Section = "local"
	SectionRemote Section = "remote"
)

// Store manages log files under root/local and root/remote.
type Store struct {
	root string
}

// New returns a Store rooted at root (created if it doesn't exist, along
// with its local/remote subdirectories).
func New(root string) (*Store, error) {
	s := &Store{root: root}
	for _, sec := range []Section{SectionLocal, SectionRemote} {
		if err := os.MkdirAll(s.dir(sec), 0o755); err != nil {
			return nil, fmt.Errorf("creating log directory for %s: %w", sec, err)
		}
	}
	return s, nil
}

func (s *Store) dir(section Section) string {
	return filepath.Join(s.root, string(section))
}

// safeNamePattern matches characters safe to put in a filename derived
// from a server/pair label — anything else gets dropped, so a display
// name can't be used to escape the log directory or produce a malformed
// path.
var safeNamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeName(label string) string {
	label = safeNamePattern.ReplaceAllString(label, "-")
	label = strings.Trim(label, "-")
	if label == "" {
		label = "batch"
	}
	return label
}

// CreateBatchLog creates (and returns an open handle to) a new log file
// for one batch — label identifies the server (local) or server pair
// (remote) this batch ran against, e.g. "PBXwareMT" or
// "PBXwareMT-to-PBXwareCC". The caller writes the batch's content and
// must Close the file when done.
func (s *Store) CreateBatchLog(section Section, label string) (*os.File, string, error) {
	name := fmt.Sprintf("%s_%s_%s.log", safeName(label), time.Now().UTC().Format("20060102-150405"), randSuffix())
	path := filepath.Join(s.dir(section), name)
	f, err := os.Create(path)
	if err != nil {
		return nil, "", fmt.Errorf("creating log file: %w", err)
	}
	return f, name, nil
}

// LogInfo is one log file's metadata, as listed by List.
type LogInfo struct {
	Name      string    `json:"name"`
	Section   Section   `json:"section"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

// List returns every log file for section, newest first.
func (s *Store) List(section Section) ([]LogInfo, error) {
	entries, err := os.ReadDir(s.dir(section))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s logs: %w", section, err)
	}
	out := make([]LogInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, LogInfo{Name: e.Name(), Section: section, SizeBytes: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// path resolves name to a path strictly inside section's directory —
// name must be a bare filename (no path separators), so a request can't
// read/delete anything outside the log store.
func (s *Store) path(section Section, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("invalid log file name %q", name)
	}
	return filepath.Join(s.dir(section), name), nil
}

// Read returns one log file's full content.
func (s *Store) Read(section Section, name string) ([]byte, error) {
	path, err := s.path(section, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Delete removes one log file.
func (s *Store) Delete(section Section, name string) error {
	path, err := s.path(section, name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// DeleteAll removes every log file in both sections — used by Settings'
// "clear all logs" and as part of resetting SwarmDialer (see
// cmd/gui/handlers.go).
func (s *Store) DeleteAll() error {
	for _, sec := range []Section{SectionLocal, SectionRemote} {
		entries, err := s.List(sec)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := s.Delete(sec, e.Name); err != nil {
				return fmt.Errorf("deleting %s: %w", e.Name, err)
			}
		}
	}
	return nil
}

func randSuffix() string {
	return fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
}
