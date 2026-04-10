package feed

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Recorder writes MarketEvents to a NDJSON file (one JSON object per line)
// so they can be replayed in backtests.
type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	writer  *bufio.Writer
	path    string
}

// NewRecorder opens (or creates) a recording file for the given market.
// Files are named: <dir>/recorded_<marketID>_<date>.ndjson
func NewRecorder(dir, marketID string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create recorder dir: %w", err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	// Shorten marketID to last 16 chars to keep filenames manageable.
	short := marketID
	if len(short) > 16 {
		short = short[len(short)-16:]
	}
	path := filepath.Join(dir, fmt.Sprintf("recorded_%s_%s.ndjson", short, date))

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open recorder file: %w", err)
	}
	return &Recorder{
		file:   f,
		writer: bufio.NewWriter(f),
		path:   path,
	}, nil
}

// Record writes one event as a JSON line. Safe for concurrent use.
func (r *Recorder) Record(ev MarketEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writer.Write(data)
	r.writer.WriteByte('\n')
	r.writer.Flush()
}

// Close flushes and closes the underlying file.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writer.Flush()
	r.file.Close()
}

// Path returns the file path being written to.
func (r *Recorder) Path() string { return r.path }

// LoadRecording reads an NDJSON recording file back into a slice of MarketEvents.
func LoadRecording(path string) ([]MarketEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []MarketEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB per line
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev MarketEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip malformed lines
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}
