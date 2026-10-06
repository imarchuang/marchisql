package storage

import (
	"bufio"
	"bytes"
	"io"
)

// scanJSONL reads newline-delimited JSON from r, delivering each complete
// line to fn, and returns the offset just past the last complete line.
//
// A trailing partial line is not delivered: every record we write ends with
// '\n' in a single Write, so a final line without a newline is definitionally
// torn by a crash mid-append. Callers truncate the file at the returned
// offset, which is safe because the files are append-only — nothing valid can
// follow a torn tail.
func scanJSONL(r io.Reader, fn func(line []byte) error) (goodEnd int64, err error) {
	br := bufio.NewReader(r)
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr == nil {
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				if err := fn(trimmed); err != nil {
					return goodEnd, err
				}
			}
			goodEnd += int64(len(line))
			continue
		}
		if rerr == io.EOF {
			// Either clean end (len(line) == 0) or torn tail (dropped).
			return goodEnd, nil
		}
		return goodEnd, rerr
	}
}
