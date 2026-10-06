package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	// Segment is the file that holds the latest record of this version.
	// Stamps move it. Not persisted: replay learns it from the file it reads.
	Segment string `json:"-"`
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
	mu         sync.RWMutex
	dir        string
	segSize    int64
	active     *os.File
	activeName string
	activeN    int
	activeSz   int64

	// manifest is the installed segment list. Nil means "every numeric
	// NNNNNN.seg", which is the layout before the first GC.
	manifest []string

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
	if err := h.discardUnpublished(); err != nil {
		return nil, err
	}
	listed, err := h.readManifest()
	if err != nil {
		return nil, err
	}
	var segs []segmentRef
	if listed != nil {
		h.manifest = listed
		for _, name := range listed {
			seq, ok := seqOf(name)
			if !ok {
				return nil, fmt.Errorf("heap: bad manifest entry %q", name)
			}
			segs = append(segs, segmentRef{seq: seq, path: filepath.Join(dir, name), name: name})
		}
	} else {
		segs, err = h.segmentFiles()
		if err != nil {
			return nil, err
		}
	}
	for i, seg := range segs {
		if err := h.replaySegment(seg.path, i == len(segs)-1); err != nil {
			return nil, err
		}
	}
	if len(segs) > 0 {
		if err := h.openNamedLocked(segs[len(segs)-1].name); err != nil {
			return nil, err
		}
	} else if err := h.openNamedLocked("000001.seg"); err != nil {
		return nil, err
	}
	return h, nil
}

type segmentRef struct {
	seq  int
	name string
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
		n, ok := seqOf(e.Name())
		if !ok || e.Name() != fmt.Sprintf("%06d.seg", n) {
			// .publishing-* and anything else is invisible until a manifest names it.
			continue
		}
		segs = append(segs, segmentRef{seq: n, name: e.Name(), path: filepath.Join(h.dir, e.Name())})
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
		h.apply(v, filepath.Base(path))
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
func (h *Heap) apply(v Version, seg string) {
	id := versionID{key: v.Key, xmin: v.Xmin}
	if old, ok := h.byID[id]; ok {
		old.Xmax = v.Xmax
		old.Fields = v.Fields
		old.Segment = seg
		return
	}
	vv := v
	vv.Segment = seg
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
	h.apply(v, h.activeName)
	return nil
}

func (h *Heap) rollLocked() error {
	if err := h.active.Sync(); err != nil {
		return err
	}
	if err := h.active.Close(); err != nil {
		return err
	}
	name := fmt.Sprintf("%06d.seg", h.activeN+1)
	if h.manifest != nil {
		name = fmt.Sprintf(".publishing-%06d.seg", h.activeN+1)
	}
	if err := h.openNamedLocked(name); err != nil {
		return err
	}
	if h.manifest != nil {
		h.manifest = append(h.manifest, name)
		return h.installManifestLocked(h.manifest)
	}
	return nil
}

func (h *Heap) openNamedLocked(name string) error {
	seq, ok := seqOf(name)
	if !ok {
		return fmt.Errorf("heap: bad segment name %q", name)
	}
	p := filepath.Join(h.dir, name)
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
	h.activeName = name
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

// latestOther returns the newest version of key NOT written by txid —
// the one a write must confront. The transaction's own uncommitted versions
// are tracked in its write set, so they are skipped here. Nil if the key
// has never existed (or only the transaction itself has written it).
func (h *Heap) latestOther(key string, excludeTxid uint64) *Version {
	h.mu.RLock()
	defer h.mu.RUnlock()
	chain := h.byKey[key]
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Xmin != excludeTxid {
			return chain[i]
		}
	}
	return nil
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
