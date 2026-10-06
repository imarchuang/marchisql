package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultSegmentSize bounds a single heap segment before rolling to the next.
const DefaultSegmentSize = 64 << 20 // 64 MiB

// Version is one physical row version — the unit of MVCC.
//
// Xmin is the txid that created the version. Xmax is the txid that superseded
// it (by update or delete); Xmax == 0 means "alive". Whether a reader can see
// this version is a pure function of these two fields, the reader's snapshot,
// and the CLOG (milestone 2).
type Version struct {
	Key    string          `json:"key"`
	Fields json.RawMessage `json:"fields"` // opaque JSON object
	Xmin   uint64          `json:"xmin"`
	Xmax   uint64          `json:"xmax"`
}

// versionID identifies a version for last-wins replay: a transaction writes
// at most one version of a key, so (key, xmin) is unique.
type versionID struct {
	key  string
	xmin uint64
}

// Heap is the versioned row store: append-only segment files of version
// records, one JSON object per line.
//
// The heap never rewrites bytes in place. Stamping an old version's xmax (the
// core of UPDATE/DELETE, milestone 4) re-appends the full record with the new
// xmax; on replay, later records for the same (key, xmin) supersede earlier
// ones. PostgreSQL instead mutates the tuple header in place — see
// VERSIONS.md for the trade-off. Dead lines are reclaimed by GC segment
// rewrites (milestone 6).
type Heap struct {
	mu       sync.RWMutex
	dir      string
	segSize  int64
	active   *os.File
	activeN  int
	activeSz int64

	versions []*Version            // live set in append order (oldest first)
	byKey    map[string][]*Version // key → version chain, oldest first
	byID     map[versionID]*Version
}

// OpenHeap opens (creating if necessary) the heap under dir, replays all
// segments in order, and truncates a torn tail left by a crash mid-append.
func OpenHeap(dir string, segSize int64) (*Heap, error) {
	if segSize <= 0 {
		segSize = DefaultSegmentSize
	}
	h := &Heap{
		dir:     dir,
		segSize: segSize,
		byKey:   make(map[string][]*Version),
		byID:    make(map[versionID]*Version),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	segs, err := h.segmentFiles()
	if err != nil {
		return nil, err
	}
	for i, seg := range segs {
		if err := h.replaySegment(seg.path, i == len(segs)-1); err != nil {
			return nil, err
		}
	}
	next := 1
	if len(segs) > 0 {
		next = segs[len(segs)-1].seq
	}
	if err := h.openSegmentLocked(next); err != nil {
		return nil, err
	}
	return h, nil
}

type segmentRef struct {
	seq  int
	path string
}

func (h *Heap) segmentFiles() ([]segmentRef, error) {
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return nil, err
	}
	var segs []segmentRef
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".seg") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".seg"))
		if err != nil {
			return nil, fmt.Errorf("heap: bad segment name %q", e.Name())
		}
		segs = append(segs, segmentRef{seq: n, path: filepath.Join(h.dir, e.Name())})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })
	return segs, nil
}

func (h *Heap) replaySegment(path string, last bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	goodEnd, err := scanJSONL(f, func(line []byte) error {
		var v Version
		if err := json.Unmarshal(line, &v); err != nil {
			return fmt.Errorf("heap: corrupt record in %s: %q: %w", filepath.Base(path), line, err)
		}
		h.apply(v)
		return nil
	})
	if err != nil {
		return err
	}
	if goodEnd < fi.Size() {
		if !last {
			return fmt.Errorf("heap: sealed segment %s has %d unreadable bytes", path, fi.Size()-goodEnd)
		}
		if err := os.Truncate(path, goodEnd); err != nil {
			return err
		}
	}
	return nil
}

// apply merges a record into the in-memory indexes. A record whose (key,
// xmin) is already present is a stamp: it supersedes the earlier record's
// xmax (and fields, for same-transaction rewrites). Otherwise it is a new
// version and joins the append order and the key's chain.
func (h *Heap) apply(v Version) {
	id := versionID{key: v.Key, xmin: v.Xmin}
	if old, ok := h.byID[id]; ok {
		old.Xmax = v.Xmax
		old.Fields = v.Fields
		return
	}
	vv := v
	h.versions = append(h.versions, &vv)
	h.byKey[v.Key] = append(h.byKey[v.Key], &vv)
	h.byID[id] = &vv
}

// Append durably writes a version record — a fresh version or an xmax stamp —
// and applies it to the in-memory indexes. The record is fsynced before
// returning: the engine's WAL-free durability story rests on "heap durable
// before commit record durable". PostgreSQL pays this cost once per commit
// via the WAL and group commit instead; see VERSIONS.md.
func (h *Heap) Append(v Version) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeSz > 0 && h.activeSz+int64(len(b)) > h.segSize {
		if err := h.rollLocked(); err != nil {
			return err
		}
	}
	if _, err := h.active.Write(b); err != nil {
		return err
	}
	if err := h.active.Sync(); err != nil {
		return err
	}
	h.activeSz += int64(len(b))
	h.apply(v)
	return nil
}

func (h *Heap) rollLocked() error {
	if err := h.active.Sync(); err != nil {
		return err
	}
	if err := h.active.Close(); err != nil {
		return err
	}
	return h.openSegmentLocked(h.activeN + 1)
}

func (h *Heap) openSegmentLocked(seq int) error {
	p := filepath.Join(h.dir, fmt.Sprintf("%06d.seg", seq))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	h.active = f
	h.activeN = seq
	h.activeSz = fi.Size()
	return nil
}

// Versions returns the live version set in append order, oldest first.
//
// The returned versions are shared with the heap's indexes: do not mutate
// them. (Milestone 2's visibility scan reads them without copying — reads
// take no locks and make no copies, which is the point of MVCC.)
func (h *Heap) Versions() []*Version {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Version, len(h.versions))
	copy(out, h.versions)
	return out
}

// Chain returns the version chain for key, oldest first. The latest version
// is the last element — the one writes must confront, snapshot be damned
// (milestone 4).
func (h *Heap) Chain(key string) []*Version {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Version, len(h.byKey[key]))
	copy(out, h.byKey[key])
	return out
}

// Len reports the number of live versions.
func (h *Heap) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.versions)
}

// Close fsyncs and closes the active segment.
func (h *Heap) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.active.Sync(); err != nil {
		return err
	}
	return h.active.Close()
}
