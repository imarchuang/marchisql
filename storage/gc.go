package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultGCThreshold is the dead-version ratio above which a background pass
// rewrites segments. ForceGC uses 0, so any dead version is collected.
const DefaultGCThreshold = 0.25

// GCStats is what a collection pass did.
type GCStats struct {
	Collected int    `json:"collected"`
	Segments  int    `json:"segments"`
	Horizon   uint64 `json:"horizon"`
}

// StartGC runs a collection pass every interval until Close. The ratio is
// the dead-version fraction above which segments are rewritten.
func (e *Engine) StartGC(interval time.Duration, threshold float64) {
	if e.gcStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	e.gcStop = stop
	e.gcDone = done
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_, _ = e.GC(threshold)
			}
		}
	}()
}

// ForceGC collects every version that is dead at the current horizon,
// ignoring the dead-ratio threshold.
func (e *Engine) ForceGC() (GCStats, error) {
	return e.GC(0)
}

// GC drops versions whose xmax is committed and older than every open
// snapshot, then rewrites the heap when the dead ratio is above threshold.
//
// The horizon is min(open snapshot xmin), or the next txid when nothing is
// open. A version with xmax committed and xmax < horizon cannot be visible
// to any current or future snapshot, so it is garbage.
func (e *Engine) GC(threshold float64) (GCStats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := GCStats{Horizon: e.horizonLocked()}
	vs := e.Heap.Versions()
	if len(vs) == 0 {
		return st, nil
	}
	live := make([]*Version, 0, len(vs))
	for _, v := range vs {
		if e.dead(v, st.Horizon) {
			st.Collected++
			continue
		}
		live = append(live, v)
	}
	if st.Collected == 0 {
		return st, nil
	}
	if float64(st.Collected)/float64(len(vs)) <= threshold {
		st.Collected = 0
		return st, nil
	}
	n, err := e.Heap.rewrite(live)
	if err != nil {
		st.Collected = 0
		return st, err
	}
	st.Segments = n
	return st, nil
}

func (e *Engine) horizonLocked() uint64 {
	if len(e.open) == 0 {
		return e.Tx.NextTxid()
	}
	var min uint64
	first := true
	for _, tx := range e.open {
		if first || tx.Snap.Xmin < min {
			min = tx.Snap.Xmin
			first = false
		}
	}
	return min
}

func (e *Engine) dead(v *Version, horizon uint64) bool {
	if v.Xmax == 0 {
		return false
	}
	if e.Tx.Status(v.Xmax) != StatusCommitted {
		return false
	}
	return v.Xmax < horizon
}

func seqOf(name string) (int, bool) {
	base := strings.TrimPrefix(name, ".publishing-")
	if !strings.HasSuffix(base, ".seg") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(base, ".seg"))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (h *Heap) readManifest() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(h.dir, "MANIFEST"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// discardUnpublished drops a manifest swap that never committed, and drops
// segment files a committed manifest no longer names.
func (h *Heap) discardUnpublished() error {
	listed, err := h.readManifest()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, n := range listed {
		keep[n] = true
	}
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		switch {
		case name == ".publishing-MANIFEST":
			if err := os.Remove(filepath.Join(h.dir, name)); err != nil {
				return err
			}
		case strings.HasPrefix(name, ".publishing-") && strings.HasSuffix(name, ".seg") && !keep[name]:
			if err := os.Remove(filepath.Join(h.dir, name)); err != nil {
				return err
			}
		case listed != nil && strings.HasSuffix(name, ".seg") && !keep[name]:
			if err := os.Remove(filepath.Join(h.dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Heap) installManifestLocked(names []string) error {
	body := strings.Join(names, "\n")
	if len(names) > 0 {
		body += "\n"
	}
	tmp := filepath.Join(h.dir, ".publishing-MANIFEST")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(h.dir, "MANIFEST")); err != nil {
		return err
	}
	if err := syncDir(h.dir); err != nil {
		return err
	}
	h.manifest = append([]string(nil), names...)
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// rewrite replaces the heap with live, via a manifest swap. The new segments
// are written as .publishing-* and become visible only when MANIFEST is
// renamed into place. Caller must not hold h.mu.
func (h *Heap) rewrite(live []*Version) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	names, err := h.writePublishing(live, h.activeN+1)
	if err != nil {
		h.removeNames(names)
		return 0, err
	}
	if len(names) == 0 {
		name := fmt.Sprintf(".publishing-%06d.seg", h.activeN+1)
		f, err := os.OpenFile(filepath.Join(h.dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return 0, err
		}
		f.Close()
		names = []string{name}
	}
	if err := h.installManifestLocked(names); err != nil {
		h.removeNames(names)
		return 0, err
	}
	if err := h.active.Sync(); err != nil {
		return 0, err
	}
	if err := h.active.Close(); err != nil {
		return 0, err
	}
	if err := h.deleteUnlistedLocked(names); err != nil {
		return 0, err
	}
	h.rebuildLocked(live)
	if err := h.openNamedLocked(names[len(names)-1]); err != nil {
		return 0, err
	}
	return len(names), nil
}

func (h *Heap) writePublishing(live []*Version, start int) ([]string, error) {
	if len(live) == 0 {
		return nil, nil
	}
	var (
		names []string
		buf   []byte
		seq   = start
	)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		name := fmt.Sprintf(".publishing-%06d.seg", seq)
		f, err := os.OpenFile(filepath.Join(h.dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		names = append(names, name)
		seq++
		buf = nil
		return nil
	}
	for _, v := range live {
		b, err := jsonMarshal(v)
		if err != nil {
			return names, err
		}
		if h.segSize > 0 && len(buf) > 0 && int64(len(buf)+len(b)) > h.segSize {
			if err := flush(); err != nil {
				return names, err
			}
		}
		v.Segment = fmt.Sprintf(".publishing-%06d.seg", seq)
		buf = append(buf, b...)
	}
	if err := flush(); err != nil {
		return names, err
	}
	return names, nil
}

func (h *Heap) removeNames(names []string) {
	for _, name := range names {
		_ = os.Remove(filepath.Join(h.dir, name))
	}
}

func (h *Heap) deleteUnlistedLocked(keep []string) error {
	listed := map[string]bool{}
	for _, n := range keep {
		listed[n] = true
	}
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".seg") || listed[name] {
			continue
		}
		if err := os.Remove(filepath.Join(h.dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (h *Heap) rebuildLocked(live []*Version) {
	h.versions = live
	h.byKey = make(map[string][]*Version, len(live))
	h.byID = make(map[versionID]*Version, len(live))
	for _, v := range live {
		h.byKey[v.Key] = append(h.byKey[v.Key], v)
		h.byID[versionID{key: v.Key, xmin: v.Xmin}] = v
	}
}
