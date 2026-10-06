package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// TxStatus is the commit status of a transaction, as tracked by the CLOG.
type TxStatus uint8

const (
	// StatusUnknown means the txid was never issued.
	StatusUnknown TxStatus = iota
	// StatusInProgress means the transaction began but has not yet ended.
	StatusInProgress
	// StatusCommitted means the transaction's versions may be visible,
	// subject to the visibility rules (milestone 2).
	StatusCommitted
	// StatusAborted means the transaction's versions are invisible to
	// everyone, forever.
	StatusAborted
)

func (s TxStatus) String() string {
	switch s {
	case StatusInProgress:
		return "in_progress"
	case StatusCommitted:
		return "committed"
	case StatusAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

func (s TxStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *TxStatus) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	switch str {
	case "in_progress":
		*s = StatusInProgress
	case "committed":
		*s = StatusCommitted
	case "aborted":
		*s = StatusAborted
	default:
		return fmt.Errorf("unknown tx status %q", str)
	}
	return nil
}

// clogRecord is one line in clog.jsonl: a transaction status transition.
type clogRecord struct {
	Txid   uint64   `json:"txid"`
	Status TxStatus `json:"status"`
}

// Clog is the commit-status log: the source of truth for which transactions
// exist and how they ended. PostgreSQL keeps this in pg_xact (formerly
// pg_clog); we keep an in-memory map persisted as JSONL, fsynced on every
// transition.
//
// Durability ordering matters because there is no WAL:
//
//  1. Begin records (in_progress) are fsynced BEFORE the txid is handed out,
//     so a version's xmin can never reference a txid the CLOG doesn't know.
//  2. Heap records are fsynced by Heap.Append before any commit record, so a
//     committed txid never points at lost versions.
//
// The commit record itself is the atomic "publish" step — that is
// PostgreSQL's "commit is just a CLOG write", in miniature.
type Clog struct {
	mu  sync.RWMutex
	f   *os.File
	m   map[uint64]TxStatus
	max uint64
}

// OpenClog opens (creating if necessary) the clog under dir, replays it, and
// truncates any torn tail left by a crash mid-append.
func OpenClog(dir string) (*Clog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "clog.jsonl")
	c := &Clog{m: make(map[uint64]TxStatus)}
	if err := c.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	c.f = f
	return c, nil
}

func (c *Clog) replay(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	goodEnd, err := scanJSONL(f, func(line []byte) error {
		var rec clogRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("clog: corrupt record %q: %w", line, err)
		}
		c.m[rec.Txid] = rec.Status
		if rec.Txid > c.max {
			c.max = rec.Txid
		}
		return nil
	})
	if err != nil {
		return err
	}
	if goodEnd < fi.Size() {
		// A torn tail means the final transition never happened; recovery
		// treats the transaction as never having reached that state.
		if err := os.Truncate(path, goodEnd); err != nil {
			return err
		}
	}
	return nil
}

// Record appends a status transition and fsyncs it before returning.
func (c *Clog) Record(txid uint64, s TxStatus) error {
	b, err := json.Marshal(clogRecord{Txid: txid, Status: s})
	if err != nil {
		return err
	}
	b = append(b, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.f.Write(b); err != nil {
		return err
	}
	if err := c.f.Sync(); err != nil {
		return err
	}
	c.m[txid] = s
	if txid > c.max {
		c.max = txid
	}
	return nil
}

// Status reports the last recorded status of txid.
func (c *Clog) Status(txid uint64) TxStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[txid]
}

// Max is the highest txid ever recorded — the basis for recovering the
// transaction counter after a restart.
func (c *Clog) Max() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.max
}

// InProgress returns the txids whose last recorded status is in_progress,
// in ascending order. Recovery uses it to abort crash victims.
func (c *Clog) InProgress() []uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []uint64
	for txid, s := range c.m {
		if s == StatusInProgress {
			out = append(out, txid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Close fsyncs and closes the log.
func (c *Clog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.f.Sync(); err != nil {
		return err
	}
	return c.f.Close()
}
