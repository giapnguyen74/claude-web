package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Tailer tails a JSONL file, emitting each parsed line to Events.
// It uses 50 ms polling with a byte-offset cursor to read only new data.
type Tailer struct {
	path     string
	offset   int64
	lineBuf  string
	Events   chan json.RawMessage
	done     chan struct{}
	stopOnce sync.Once
}

func newTailer(path string) *Tailer {
	return &Tailer{
		path:   path,
		Events: make(chan json.RawMessage, 512),
		done:   make(chan struct{}),
	}
}

// readJSONLines reads a JSONL file and returns every valid JSON line in order.
// Missing files yield an empty slice. Safe to call concurrently with appends.
func readJSONLines(path string) []json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	return out
}

// ReadAll reads every event from the beginning of the file.
// It is independent of the live-tail offset and safe to call concurrently.
func (t *Tailer) ReadAll() []json.RawMessage {
	return readJSONLines(t.path)
}

// Start begins polling from the current end of file (live tail only).
// Historical events are available via ReadAll.
func (t *Tailer) Start() {
	// Initialise offset to current EOF so we only pick up NEW lines.
	if fi, err := os.Stat(t.path); err == nil {
		t.offset = fi.Size()
	}
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		// This goroutine is the only sender on Events, so closing it here
		// (after the loop exits) lets consumers ranging over Events finish.
		defer close(t.Events)
		for {
			select {
			case <-ticker.C:
				t.poll()
			case <-t.done:
				t.poll() // final flush so the closing session_end isn't dropped
				return
			}
		}
	}()
}

// Stop halts polling and is safe to call multiple times.
func (t *Tailer) Stop() { t.stopOnce.Do(func() { close(t.done) }) }

func (t *Tailer) poll() {
	fi, err := os.Stat(t.path)
	if err != nil {
		return
	}
	if fi.Size() < t.offset {
		// File was truncated / rotated
		t.offset = 0
		t.lineBuf = ""
	}
	if fi.Size() == t.offset {
		return
	}

	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	t.offset = fi.Size()
	t.lineBuf += string(data)

	for {
		idx := strings.Index(t.lineBuf, "\n")
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(t.lineBuf[:idx])
		t.lineBuf = t.lineBuf[idx+1:]
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		select {
		case t.Events <- json.RawMessage(line):
		default:
			// channel full — drop oldest by draining one, then retry
			select {
			case <-t.Events:
			default:
			}
			select {
			case t.Events <- json.RawMessage(line):
			default:
			}
		}
	}
}
