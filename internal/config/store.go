package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// File names inside the data directory.
const (
	FileName       = "config.json"
	BackupFileName = "config.json.bak"
	KeyFileName    = "master.key"
)

// Subscriber is notified after every successful update with the previous and
// new snapshot. Subscribers must not call Store.Update synchronously.
type Subscriber func(old, new *Config)

// Store holds the current configuration as an immutable snapshot and persists
// changes atomically (write temp file, fsync, rename; previous file kept as
// config.json.bak).
type Store struct {
	dir  string
	path string

	mu       sync.Mutex // serialises writers
	notifyMu sync.Mutex // keeps notifications in commit order
	cur      atomic.Pointer[Config]
	rev      atomic.Uint64

	subsMu sync.Mutex
	subs   []Subscriber
}

// OpenResult describes how the configuration was obtained.
type OpenResult struct {
	Fresh      bool   // no configuration existed
	FromBackup bool   // config.json was missing/corrupt, backup used
	Archived   string // path of an archived pre-migration copy, if any
	Report     *LoadReport
}

// Open loads (or initialises) the configuration in dataDir.
func Open(dataDir string) (*Store, *OpenResult, error) {
	s := &Store{dir: dataDir, path: filepath.Join(dataDir, FileName)}
	res := &OpenResult{}
	data, err := os.ReadFile(s.path)
	var c *Config
	switch {
	case err == nil:
		cfg, rep, perr := Parse(data, dataDir)
		if perr != nil {
			// corrupt main file: fall back to backup, keep the corrupt file for analysis
			bak, bErr := os.ReadFile(filepath.Join(dataDir, BackupFileName))
			if bErr != nil {
				return nil, nil, fmt.Errorf("%s ist beschädigt (%v) und kein lesbares Backup vorhanden", s.path, perr)
			}
			cfg, rep, bErr = Parse(bak, dataDir)
			if bErr != nil {
				return nil, nil, fmt.Errorf("%s und %s sind beschädigt: %v", FileName, BackupFileName, perr)
			}
			corrupt := s.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
			_ = os.WriteFile(corrupt, data, 0o600)
			res.FromBackup = true
			data = bak
		}
		c, res.Report = cfg, rep
		if rep.SourceVersion < SchemaVersion {
			arch := filepath.Join(dataDir, fmt.Sprintf("config.json.v%d-original", rep.SourceVersion))
			if _, statErr := os.Stat(arch); errors.Is(statErr, fs.ErrNotExist) {
				if wErr := writeAtomic(arch, data, 0o600); wErr == nil {
					res.Archived = arch
				}
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		bak, bErr := os.ReadFile(filepath.Join(dataDir, BackupFileName))
		if bErr == nil {
			cfg, rep, pErr := Parse(bak, dataDir)
			if pErr == nil {
				c, res.Report, res.FromBackup = cfg, rep, true
				break
			}
		}
		c = Default(dataDir)
		res.Fresh = true
		res.Report = &LoadReport{SourceVersion: SchemaVersion}
	default:
		return nil, nil, err
	}
	s.cur.Store(c)
	s.rev.Store(1)
	// Persist the normalised form only when it differs from what is on disk.
	out, err := marshal(c)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(data)) {
		if err := s.persist(out); err != nil {
			return nil, nil, fmt.Errorf("Konfiguration kann nicht gespeichert werden: %w", err)
		}
	}
	return s, res, nil
}

// NewMemoryStore creates a store that is not backed by a file (tests).
func NewMemoryStore(c *Config) *Store {
	s := &Store{}
	s.cur.Store(c)
	s.rev.Store(1)
	return s
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// Get returns the current immutable snapshot. Callers must not modify it.
func (s *Store) Get() *Config { return s.cur.Load() }

// Revision increases with every committed update.
func (s *Store) Revision() uint64 { return s.rev.Load() }

// Subscribe registers fn for change notifications.
func (s *Store) Subscribe(fn Subscriber) {
	s.subsMu.Lock()
	s.subs = append(s.subs, fn)
	s.subsMu.Unlock()
}

// Update applies fn to a deep copy of the current config, normalises,
// persists and publishes it. If fn returns an error nothing changes.
func (s *Store) Update(fn func(c *Config) error) (*Config, error) {
	s.mu.Lock()
	old := s.cur.Load()
	next := old.Clone()
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	Normalize(next, s.dir)
	if s.path != "" {
		out, err := marshal(next)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if err := s.persist(out); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("Konfiguration konnte nicht gespeichert werden: %w", err)
		}
	}
	s.cur.Store(next)
	s.rev.Add(1)
	s.notifyMu.Lock()
	s.mu.Unlock()
	s.subsMu.Lock()
	subs := append([]Subscriber(nil), s.subs...)
	s.subsMu.Unlock()
	for _, fn := range subs {
		fn(old, next)
	}
	s.notifyMu.Unlock()
	return next, nil
}

func marshal(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// persist writes the config atomically and keeps the previous version as backup.
func (s *Store) persist(data []byte) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}
	if prev, err := os.ReadFile(s.path); err == nil && len(bytes.TrimSpace(prev)) > 0 && json.Valid(prev) {
		if err := writeAtomic(filepath.Join(s.dir, BackupFileName), prev, 0o600); err != nil {
			return err
		}
	}
	return writeAtomic(s.path, data, 0o600)
}

// writeAtomic writes data to path via a synced temporary file and rename, so a
// crash leaves either the old or the new content, never a truncated file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil && !isUnsupported(err) {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

// WriteFileAtomic exposes the atomic writer for other packages (e.g. go2rtc config).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, data, perm)
}
