package drift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/yuuki/hostping/internal/state"
)

// rejectedSuffix names the one file an unusable history is set aside as.
const rejectedSuffix = ".rejected"

// maxFileBytes bounds what Load reads: several times the largest history
// the tracker writes (MaxPaths summaries of a few kilobytes each).
const maxFileBytes = 32 << 20

// LoadStatus is what Store.Load found.
type LoadStatus int

const (
	// LoadFresh: there is no history yet. Tracking starts an empty one.
	LoadFresh LoadStatus = iota
	// LoadRestored: the history was read.
	LoadRestored
	// LoadSetAside: the file was not a usable history (truncated, corrupt,
	// not written by hostping) and was renamed to RejectedPath, replacing an
	// earlier one. Tracking starts an empty history.
	LoadSetAside
	// LoadBlocked: the file can be neither used nor replaced without losing
	// a history that may be good: a newer build wrote it, it cannot be
	// read, or it could not be set aside. It is left untouched and tracking
	// stays off.
	LoadBlocked
)

// Store reads and writes the drift history of one host. Unlike the state
// file next to it, the history is not tied to a boot or to a topology: it is
// evidence about hardware over months, kept per path.
type Store struct {
	path string
}

// NewStore is the store of the history in dir, which the caller has checked
// is writable (state.Open).
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, FileName)}
}

// Path is the history's path.
func (s *Store) Path() string { return s.path }

// RejectedPath is where an unusable history is set aside.
func (s *Store) RejectedPath() string { return s.path + rejectedSuffix }

// Load reads the history. why says, for every status but LoadRestored, what
// was found. Only LoadFresh, LoadRestored and LoadSetAside let tracking run,
// on the returned file; LoadBlocked never writes.
func (s *Store) Load() (f File, status LoadStatus, why string) {
	b, err := readBounded(s.path, maxFileBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return File{}, LoadFresh, "no history yet"
	case errors.Is(err, errTooLarge):
		return s.setAside(fmt.Sprintf("larger than %d bytes", maxFileBytes))
	case err != nil:
		return File{}, LoadBlocked, fmt.Sprintf("unreadable: %v", err)
	}
	// The version is read on its own first: a newer build's file has fields
	// the strict parser would reject, and it must not be set aside.
	var head struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(b, &head); err == nil && head.Version != nil && *head.Version > Version {
		return File{}, LoadBlocked, fmt.Sprintf("schema version %d, written by a newer build (this build reads %d)", *head.Version, Version)
	}
	f, err = Parse(b)
	if err != nil {
		return s.setAside(err.Error())
	}
	return f, LoadRestored, ""
}

// setAside renames the history to RejectedPath, so that a file this build
// cannot use is kept for the operator instead of being overwritten by the
// next save.
func (s *Store) setAside(why string) (File, LoadStatus, string) {
	if err := os.Rename(s.path, s.RejectedPath()); err != nil {
		return File{}, LoadBlocked, fmt.Sprintf("%s, and it could not be set aside: %v", why, err)
	}
	return File{}, LoadSetAside, why
}

// Save writes f, stamped with now, replacing the history atomically
// (state.WriteAtomic).
func (s *Store) Save(f File, now time.Time) error {
	f.Version, f.SavedAt = Version, now.UTC()
	b, err := Marshal(f)
	if err != nil {
		return err
	}
	return state.WriteAtomic(s.path, b)
}

var errTooLarge = errors.New("file too large")

// readBounded reads the file at path, or fails with errTooLarge when it
// holds more than limit bytes.
func readBounded(path string, limit int64) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}
