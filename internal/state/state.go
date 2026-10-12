// Package state keeps the daemon's flapping evidence across a restart
// (--state-dir): the per-link consecutive-gray counters of NSDI'23 §4.3, the
// per-GPU streaks of slow memory registration (gpu_mr_registration_slow), the
// per-path streaks of sibling latency verdicts (path_latency_abnormal) and the
// end of the last idle-mode run, so a slowly flapping link is still
// reported on a fleet whose agents restart, and a restart does not pull the
// next idle full-mesh run forward.
//
// The state is one small JSON file, replaced atomically. It is only evidence
// about the links of this boot of this host: a file written for another
// topology, before a reboot (which retrains every link), or longer ago than
// one flap window of idle runs is ignored, never merged.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/topology"
)

// Version is the schema version of the state file. A file with any other
// version is ignored rather than migrated: it only holds a few runs' worth of
// evidence, which the daemon gathers again.
const Version = 1

// FileName is the state file inside --state-dir.
const FileName = "state.json"

// fileMode is the state file's permission: it names the host's devices and
// which of its links look unhealthy.
const fileMode = 0o600

// BootIDPath is where Linux exposes the identity of the running boot.
const BootIDPath = "/proc/sys/kernel/random/boot_id"

// File is the content of the state file.
type File struct {
	Version int `json:"version"`
	// Topology is Fingerprint of the graph the counters were taken on.
	Topology string `json:"topology"`
	// BootID is the kernel boot ID when the file was written.
	BootID string `json:"boot_id"`
	// SavedAt is when the file was written.
	SavedAt time.Time `json:"saved_at"`
	// Gray is the engine's consecutive-gray counter per link ID, exactly as
	// Engine.SnapshotGray returns it.
	Gray map[string]int `json:"gray"`
	// SlowMR is the engine's count of consecutive idle runs with slow memory
	// registration per GPU endpoint ID, as Engine.SnapshotStreaks returns
	// it. It is optional: a file written before it existed restores with no
	// streaks, which only delays a report by one idle run.
	SlowMR map[string]int `json:"slow_mr,omitempty"`
	// Latency is the engine's count of consecutive idle runs that read a
	// path latency-abnormal against its siblings, keyed "rnic->endpoint", as
	// Engine.SnapshotStreaks returns it. Optional like SlowMR: a file written
	// before it existed restores with no streaks.
	Latency map[string]int `json:"latency,omitempty"`
	// GrayUpdatedAt is the end of the last engine run whose result is in Gray.
	GrayUpdatedAt time.Time `json:"gray_updated_at,omitzero"`
	// LastIdleEnd is the end of the last idle-trigger run that was confirmed
	// or discarded; the next one is due --idle-interval after it.
	LastIdleEnd time.Time `json:"last_idle_end,omitzero"`
}

// LastTest is the newest run the file records, which its age is measured
// from: zero when it records none.
func (f File) LastTest() time.Time {
	if f.LastIdleEnd.After(f.GrayUpdatedAt) {
		return f.LastIdleEnd
	}
	return f.GrayUpdatedAt
}

// Fingerprint is a stable hash of what the gray counters refer to: every RNIC
// (ID and verbs device), endpoint (ID and kind), link (ID and kind) and path
// (RNIC, endpoint, distance and ordered link IDs), each sorted. Capacities,
// line rates, negotiated speeds, PCIe addresses and rail keys are left out:
// they are re-read on every start and may legitimately read differently,
// while a counter keyed by link ID stays meaningful as long as the same links
// sit on the same paths. A changed fingerprint means the counters may name
// links that no longer exist or now lie on other paths.
func Fingerprint(g topology.Graph) string {
	var lines []string
	for _, n := range g.RNICs {
		lines = append(lines, "rnic\x00"+n.ID+"\x00"+n.Device)
	}
	for _, e := range g.Endpoints {
		lines = append(lines, "endpoint\x00"+e.ID+"\x00"+string(e.Kind))
	}
	for _, l := range g.Links {
		lines = append(lines, "link\x00"+l.ID+"\x00"+l.Kind)
	}
	for _, p := range g.Paths {
		lines = append(lines, "path\x00"+p.RNIC+"\x00"+p.Endpoint+"\x00"+
			strconv.Itoa(int(p.Distance))+"\x00"+strings.Join(p.LinkIDs, "\x01"))
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// KernelBootID reads the running kernel's boot ID.
func KernelBootID() (string, error) {
	return readBootID(BootIDPath)
}

func readBootID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return id, nil
}

// Store reads and writes the state file of one daemon: one boot, one graph.
type Store struct {
	path     string
	bootID   string
	topology string
}

// Open prepares dir for the state file, creating it 0700 if missing, and
// checks that a file can be created in it, so a wrong or read-only
// --state-dir fails at start-up rather than at the first save.
func Open(dir, bootID, fingerprint string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("empty state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".probe-*.tmp")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, FileName), bootID: bootID, topology: fingerprint}, nil
}

// Path is the state file's path.
func (s *Store) Path() string { return s.path }

// Load reads the state file and checks that it may be restored at now. It
// returns ok=false with the reason when it may not: no file, an unreadable or
// corrupt one, another schema version, topology or boot, or a last run older
// than maxAge. None of these is an error to the caller; the daemon starts
// from empty state as it did before --state-dir existed.
func (s *Store) Load(now time.Time, maxAge time.Duration) (f File, ok bool, why string) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, "no state file yet"
	}
	if err != nil {
		return File{}, false, fmt.Sprintf("unreadable: %v", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, false, fmt.Sprintf("corrupt: %v", err)
	}
	switch {
	case f.Version != Version:
		return File{}, false, fmt.Sprintf("schema version %d, this build reads %d", f.Version, Version)
	case f.Topology != s.topology:
		return File{}, false, "the discovered topology changed"
	case f.BootID != s.bootID:
		return File{}, false, "the host rebooted since it was written (links retrain on boot)"
	}
	last := f.LastTest()
	if last.IsZero() {
		return File{}, false, "it records no run"
	}
	if age := now.Sub(last); age > maxAge {
		return File{}, false, fmt.Sprintf("its last run ended %v ago, more than --flap-window x --idle-interval (%v)", age.Round(time.Second), maxAge)
	}
	if f.Gray == nil {
		f.Gray = map[string]int{}
	}
	if f.SlowMR == nil {
		f.SlowMR = map[string]int{}
	}
	if f.Latency == nil {
		f.Latency = map[string]int{}
	}
	return f, true, ""
}

// Save writes f, stamped with this store's version, topology, boot ID and
// now, replacing the state file atomically: a temporary file in the same
// directory is written, synced and renamed over it, and the directory is
// synced. A failure leaves the previous file as it was and no temporary file
// behind.
func (s *Store) Save(f File, now time.Time) error {
	f.Version, f.Topology, f.BootID, f.SavedAt = Version, s.topology, s.bootID, now.UTC()
	f.GrayUpdatedAt, f.LastIdleEnd = utc(f.GrayUpdatedAt), utc(f.LastIdleEnd)
	if f.Gray == nil {
		f.Gray = map[string]int{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "."+FileName+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	done = true
	// The rename is durable once the directory entry is; a directory that
	// cannot be synced (some filesystems refuse) does not undo it.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
