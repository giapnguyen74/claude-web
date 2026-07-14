package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// ── ActiveProject ─────────────────────────────────────────────────────────

// ActiveProject bundles all runtime state for a single running Claude instance.
type ActiveProject struct {
	Project    Project
	State      *State
	Proc       *claudeProc
	Tailer     *Tailer
	Hub        *hub
	InputQueue chan inputJob
	SessionDir string
	EventsPath string
	InputPath  string

	done     chan struct{} // closed to stop the input worker and unblock senders
	stopOnce sync.Once
}

// shutdown stops the input worker and unblocks any pending senders.
// It is safe to call multiple times.
func (ap *ActiveProject) shutdown() {
	ap.stopOnce.Do(func() { close(ap.done) })
}

// sendInput enqueues a command for the running process and waits for the
// worker's result. It returns an error if the session is shutting down,
// so callers never block forever on a stopped worker (and never send on a
// queue whose consumer has exited).
func (ap *ActiveProject) sendInput(data any) error {
	respCh := make(chan error, 1)
	select {
	case ap.InputQueue <- inputJob{data: data, respCh: respCh}:
	case <-ap.done:
		return fmt.Errorf("session is not running")
	}
	select {
	case err := <-respCh:
		return err
	case <-ap.done:
		return fmt.Errorf("session is not running")
	}
}

// ── ProcManager ───────────────────────────────────────────────────────────

// ProcManager manages multiple concurrent Claude processes, one per project.
type ProcManager struct {
	mu       sync.RWMutex
	active   map[string]*ActiveProject // keyed by project ID
	starting map[string]bool           // project IDs reserved mid-start
}

// NewProcManager creates a new process manager.
func NewProcManager() *ProcManager {
	return &ProcManager{
		active:   make(map[string]*ActiveProject),
		starting: make(map[string]bool),
	}
}

// Start spawns a Claude process for the given project.
//
// The project ID is reserved atomically (in `active` or `starting`) before the
// slow spawn work begins, so two concurrent Start calls for the same project
// can never both launch a process.
func (pm *ProcManager) Start(proj Project, resolvedArgs []string) (*ActiveProject, error) {
	pm.mu.Lock()
	if _, exists := pm.active[proj.ID]; exists {
		pm.mu.Unlock()
		return nil, fmt.Errorf("project %s is already running", proj.Name)
	}
	if pm.starting[proj.ID] {
		pm.mu.Unlock()
		return nil, fmt.Errorf("project %s is already starting", proj.Name)
	}
	pm.starting[proj.ID] = true
	pm.mu.Unlock()

	ap, err := pm.spawnActive(proj, resolvedArgs)

	pm.mu.Lock()
	delete(pm.starting, proj.ID)
	if err == nil {
		pm.active[proj.ID] = ap
	}
	pm.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return ap, nil
}

// spawnActive does the actual process launch and wiring. The caller is
// responsible for the `starting`/`active` bookkeeping in ProcManager.
func (pm *ProcManager) spawnActive(proj Project, resolvedArgs []string) (*ActiveProject, error) {
	// Set up session files
	sf, err := ensureSessionFiles(proj.Path)
	if err != nil {
		return nil, fmt.Errorf("session files: %w", err)
	}

	// Archive the previous session's events, then reset events.jsonl so the live
	// channel always starts clean — a stale session_end (the translated `result`
	// event) must never leak into the new channel and end it prematurely. Prior
	// conversations stay browsable via the history API, which reads the archive.
	if histPath, err := historyPathForProject(proj.Path); err == nil {
		if err := archiveEvents(sf.eventsPath, histPath); err != nil {
			fmt.Fprintf(os.Stderr, "[procmgr] archive events warning: %v\n", err)
		}
	}
	if err := os.WriteFile(sf.eventsPath, nil, 0o600); err != nil {
		return nil, fmt.Errorf("init events file: %w", err)
	}
	if err := os.WriteFile(sf.inputPath, nil, 0o600); err != nil {
		return nil, fmt.Errorf("init input file: %w", err)
	}

	// Spawn claude
	fmt.Fprintf(os.Stderr, "[procmgr] Starting Claude for project %q in %s\n", proj.Name, proj.Path)
	proc, err := spawnClaude(spawnOptions{
		projectDir: proj.Path,
		eventsPath: sf.eventsPath,
		inputPath:  sf.inputPath,
		extraArgs:  resolvedArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn claude: %w", err)
	}

	state := newState()
	state.setProcess(proc)
	state.setRunning("starting-session")

	ap := &ActiveProject{
		Project:    proj,
		State:      state,
		Proc:       proc,
		Tailer:     newTailer(sf.eventsPath),
		Hub:        newHub(),
		InputQueue: make(chan inputJob, 1024),
		SessionDir: sf.sessionDir,
		EventsPath: sf.eventsPath,
		InputPath:  sf.inputPath,
		done:       make(chan struct{}),
	}

	// Start input worker
	go pm.inputWorker(ap)

	// Stream stderr with [project-name] prefix
	go func() {
		scanner := bufio.NewScanner(proc.stderrPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[claude/%s] %s\n", proj.Name, scanner.Text())
		}
	}()

	// Monitor process lifecycle. proc.Wait() is the single reaper for the
	// process; kill() only waits on proc.done rather than calling Wait again.
	go func() {
		err := proc.Wait()
		state.setStopped()

		if err != nil {
			fmt.Fprintf(os.Stderr, "[procmgr] Claude for %q exited with error: %v\n", proj.Name, err)
		} else {
			fmt.Fprintf(os.Stderr, "[procmgr] Claude for %q exited successfully.\n", proj.Name)
		}

		// Broadcast status change to connected clients, then remove the dead
		// session from the active set so the project can be started again. The
		// short grace period lets the tailer flush the final events (e.g. the
		// closing session_end) that may still be in-flight from stdout.
		ap.Hub.broadcast(marshalProjectStatus(ap))
		time.Sleep(200 * time.Millisecond)
		pm.retire(proj.ID, ap)
	}()

	// Start event tailer and broadcasting
	go func() {
		for raw := range ap.Tailer.Events {
			onProjectLiveEvent(ap, raw)
		}
	}()
	ap.Tailer.Start()

	return ap, nil
}

// Stop gracefully stops the Claude process for a project.
// It first tries /exit, then falls back to kill after timeout.
func (pm *ProcManager) Stop(projectID string) error {
	pm.mu.RLock()
	ap, ok := pm.active[projectID]
	pm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("project %s is not running", projectID)
	}

	// Try graceful exit via /exit command (closes stdin in the worker).
	fmt.Fprintf(os.Stderr, "[procmgr] Sending /exit to %q\n", ap.Project.Name)
	_ = ap.sendInput(map[string]any{"type": "submit", "text": "/exit"})

	// Wait for graceful exit with timeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ { // 10 seconds
			status, _ := ap.State.get()
			if status == "stopped" {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	<-done
	status, _ := ap.State.get()
	if status != "stopped" {
		fmt.Fprintf(os.Stderr, "[procmgr] Graceful exit timed out for %q, killing\n", ap.Project.Name)
		ap.State.kill()
	}

	// Tear down background goroutines: input worker (via shutdown), then the
	// tailer poll loop and event forwarder (Tailer.Stop closes Events).
	ap.shutdown()
	ap.Tailer.Stop()

	pm.mu.Lock()
	delete(pm.active, projectID)
	pm.mu.Unlock()

	// Broadcast final status
	ap.Hub.broadcast(marshalProjectStatus(ap))

	return nil
}

// retire removes an exited session from the active set and stops its background
// goroutines, returning the project to idle so it can be started again. Safe to
// call multiple times and concurrently with Stop (delete is guarded by identity,
// shutdown and Tailer.Stop are idempotent).
func (pm *ProcManager) retire(projectID string, ap *ActiveProject) {
	pm.mu.Lock()
	if pm.active[projectID] == ap {
		delete(pm.active, projectID)
	}
	pm.mu.Unlock()
	ap.shutdown()
	ap.Tailer.Stop()
}

// GetActive returns the ActiveProject for a given project ID, or nil.
func (pm *ProcManager) GetActive(projectID string) *ActiveProject {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.active[projectID]
}

// ListActive returns all active project IDs.
func (pm *ProcManager) ListActive() map[string]*ActiveProject {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make(map[string]*ActiveProject, len(pm.active))
	for k, v := range pm.active {
		out[k] = v
	}
	return out
}

// KillAll stops all running Claude processes (used during shutdown).
func (pm *ProcManager) KillAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for id, ap := range pm.active {
		fmt.Fprintf(os.Stderr, "[procmgr] Killing Claude for %q\n", ap.Project.Name)
		ap.State.kill()
		ap.shutdown()
		ap.Tailer.Stop()
		delete(pm.active, id)
	}
}

// ── Internal helpers ──────────────────────────────────────────────────────

func (pm *ProcManager) inputWorker(ap *ActiveProject) {
	for {
		select {
		case <-ap.done:
			return
		case job := <-ap.InputQueue:
			pm.handleInputJob(ap, job)
		}
	}
}

func (pm *ProcManager) handleInputJob(ap *ActiveProject, job inputJob) {
	// 1. Log/Append to input.jsonl
	if err := appendInput(ap.InputPath, job.data); err != nil {
		job.respCh <- err
		return
	}

	// 2. Translate and write to stdin
	var writeErr error
	if m, ok := job.data.(map[string]any); ok {
		if m["type"] == "submit" && m["text"] == "/exit" {
			// Graceful exit: close stdin
			fmt.Fprintf(os.Stderr, "[procmgr] Closing stdin for project: %s\n", ap.Project.Name)
			if ap.Proc != nil && ap.Proc.stdinPipe != nil {
				writeErr = ap.Proc.stdinPipe.Close()
			}
		} else {
			// Record the user's prompt in the event timeline so the UI shows it
			// (Claude's output stream does not echo the prompt back). Only for
			// actual submits — not tool-approval confirmations.
			if m["type"] == "submit" {
				if text, ok := m["text"].(string); ok && text != "" {
					_ = appendUserEvent(ap.EventsPath, text)
				}
			}
			// Normal user turn or confirmation
			claudeInput, err := toClaudeInput(m)
			if err != nil {
				writeErr = err
			} else if len(claudeInput) > 0 {
				if ap.Proc != nil && ap.Proc.stdinPipe != nil {
					_, writeErr = ap.Proc.stdinPipe.Write(append(claudeInput, '\n'))
				} else {
					writeErr = fmt.Errorf("stdin pipe not available")
				}
			}
		}
	}
	job.respCh <- writeErr
}

func toClaudeInput(qwenStyle map[string]any) ([]byte, error) {
	t, ok := qwenStyle["type"].(string)
	if !ok {
		return nil, fmt.Errorf("missing type in input")
	}

	switch t {
	case "submit":
		text, _ := qwenStyle["text"].(string)
		if text == "/exit" {
			return nil, nil // Handled separately by closing stdin
		}
		// Convert to user message
		type contentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		type messageBody struct {
			Role    string         `json:"role"`
			Content []contentBlock `json:"content"`
		}
		type claudeSubmit struct {
			Type    string      `json:"type"`
			Message messageBody `json:"message"`
		}
		cs := claudeSubmit{
			Type: "user",
			Message: messageBody{
				Role: "user",
				Content: []contentBlock{
					{Type: "text", Text: text},
				},
			},
		}
		return json.Marshal(cs)

	case "confirmation_response":
		reqID, _ := qwenStyle["request_id"].(string)
		allowed, _ := qwenStyle["allowed"].(bool)
		type claudeConfirm struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Allowed   bool   `json:"allowed"`
		}
		cc := claudeConfirm{
			Type:      "confirmation_response",
			RequestID: reqID,
			Allowed:   allowed,
		}
		return json.Marshal(cc)
	}

	return nil, fmt.Errorf("unknown input type: %s", t)
}
