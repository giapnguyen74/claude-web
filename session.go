package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Session files ─────────────────────────────────────────────────────────

type sessionFiles struct {
	sessionDir string
	eventsPath string
	inputPath  string
}


// sessionDirForProject returns ~/.claude-code-web/sessions/<basename>_<8hexchars>/
// keyed by the absolute project path so each project gets its own slot.
func sessionDirForProject(absProjectDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Short readable name: last path component + 8-char hash for uniqueness
	base := filepath.Base(absProjectDir)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(absProjectDir)))[:8]
	name := fmt.Sprintf("%s_%s", base, hash)
	return filepath.Join(home, ".claude-code-web", "sessions", name), nil
}

// eventsPathForProject returns the events.jsonl path for a project without
// creating anything. Used by read-only history queries.
func eventsPathForProject(projectDir string) (string, error) {
	sd, err := sessionDirForProject(projectDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "events.jsonl"), nil
}

// historyPathForProject returns the append-only archive of prior sessions'
// events for a project. The live events.jsonl is reset each Start; its previous
// contents are appended here so past conversations remain browsable.
func historyPathForProject(projectDir string) (string, error) {
	sd, err := sessionDirForProject(projectDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "history.jsonl"), nil
}

// archiveEvents appends the current events file to the history archive before
// events.jsonl is reset. No-op if the events file is missing or empty.
func archiveEvents(eventsPath, historyPath string) error {
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func ensureSessionFiles(projectDir string) (sessionFiles, error) {
	sd, err := sessionDirForProject(projectDir)
	if err != nil {
		return sessionFiles{}, err
	}
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return sessionFiles{}, fmt.Errorf("mkdir session dir: %w", err)
	}

	ep := filepath.Join(sd, "events.jsonl")
	ip := filepath.Join(sd, "input.jsonl")

	for _, p := range []string{ep, ip} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				return sessionFiles{}, err
			}
		}
	}

	return sessionFiles{
		sessionDir: sd,
		eventsPath: ep,
		inputPath:  ip,
	}, nil
}

// ── Process spawn ─────────────────────────────────────────────────────────

type spawnOptions struct {
	projectDir string
	eventsPath string
	inputPath  string
	extraArgs  []string // forwarded verbatim to claude
}

type claudeProc struct {
	cmd        *exec.Cmd
	stdinPipe  io.WriteCloser
	stderrPipe io.ReadCloser
	stdoutPipe io.ReadCloser

	done     chan struct{} // closed once the process has been reaped
	waitOnce sync.Once
	waitErr  error
}

// Wait reaps the process exactly once and is safe to call from multiple
// goroutines. All callers observe the same exit error, and proc.done is
// closed when the process has been reaped.
func (p *claudeProc) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	})
	<-p.done
	return p.waitErr
}

// State holds runtime session state, safe for concurrent access.
type State struct {
	mu        sync.RWMutex
	status    string // "starting" | "running" | "stopped"
	sessionID string
	proc      *claudeProc
}

func newState() *State { return &State{status: "starting"} }

func (s *State) setProcess(p *claudeProc) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.proc = p
}

func (s *State) setRunning(id string) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.status = "running"
	s.sessionID = id
}

func (s *State) setStopped() {
	s.mu.Lock(); defer s.mu.Unlock()
	s.status = "stopped"
}

func (s *State) get() (status, sessionID string) {
	s.mu.RLock(); defer s.mu.RUnlock()
	return s.status, s.sessionID
}

func (s *State) kill() {
	s.mu.Lock()
	s.status = "stopped"
	proc := s.proc
	s.mu.Unlock()

	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}

	// Close stdin to nudge a graceful exit.
	if proc.stdinPipe != nil {
		_ = proc.stdinPipe.Close()
	}

	// Send SIGTERM, then SIGKILL if it doesn't exit in time. The process is
	// reaped solely by the monitor goroutine via proc.Wait(); here we only
	// wait on proc.done so cmd.Wait() is never called twice on the same Cmd.
	_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-proc.done:
		// Exited gracefully.
	case <-time.After(5 * time.Second):
		_ = proc.cmd.Process.Kill()
		select {
		case <-proc.done:
		case <-time.After(5 * time.Second):
			// Reaper will catch up eventually; don't block shutdown further.
		}
	}
}

// Resolved binary paths are cached after the first lookup. Resolution shells
// out to login shells (zsh/bash -l), which is slow, so we only do it once.
var (
	claudeOnce sync.Once
	claudePath string
	claudeErr  error

	nodeOnce sync.Once
	nodePath string
	nodeErr  error
)

// resolveClaude finds the claude binary path, caching the result.
func resolveClaude() (string, error) {
	claudeOnce.Do(func() {
		claudePath, claudeErr = lookupClaude()
	})
	return claudePath, claudeErr
}

// lookupClaude finds the claude binary path.
// It uses "which claude" first, then exec.LookPath, and finally falls back to common paths.
func lookupClaude() (string, error) {
	// 1. Try running "which claude" inside login shells (very robust on macOS/Linux)
	for _, shell := range []string{"zsh", "bash"} {
		cmd := exec.Command(shell, "-l", "-c", "which claude")
		if out, err := cmd.CombinedOutput(); err == nil {
			path := strings.TrimSpace(string(out))
			if path != "" && isExec(path) {
				return path, nil
			}
		}
	}

	// 2. Try exec.LookPath("claude")
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}

	// 3. Fallback to NVM or other common paths
	if home, err := os.UserHomeDir(); err == nil {
		nvmDir := os.Getenv("NVM_DIR")
		if nvmDir == "" {
			nvmDir = filepath.Join(home, ".nvm")
		}
		versionsDir := filepath.Join(nvmDir, "versions", "node")
		if entries, err := os.ReadDir(versionsDir); err == nil {
			for _, entry := range entries {
				candidate := filepath.Join(versionsDir, entry.Name(), "bin", "claude")
				if isExec(candidate) {
					return candidate, nil
				}
			}
		}
		// common fixed locations
		for _, loc := range []string{
			"/opt/homebrew/bin/claude",
			"/usr/local/bin/claude",
			filepath.Join(home, ".local", "bin", "claude"),
		} {
			if isExec(loc) {
				return loc, nil
			}
		}
	}

	return "", fmt.Errorf("claude not found in PATH or standard locations")
}

func isExec(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func isNodeScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var buf [256]byte
	n, err := f.Read(buf[:])
	if err != nil && err != io.EOF {
		return false
	}
	content := string(buf[:n])
	if strings.HasPrefix(content, "#!") {
		firstLine := content
		if idx := strings.Index(content, "\n"); idx >= 0 {
			firstLine = content[:idx]
		}
		return strings.Contains(firstLine, "node")
	}
	return false
}

// resolveNode finds the node binary path, caching the result.
func resolveNode() (string, error) {
	nodeOnce.Do(func() {
		nodePath, nodeErr = lookupNode()
	})
	return nodePath, nodeErr
}

func lookupNode() (string, error) {
	// 1. Try running "which node" inside login shells (very robust on macOS/Linux)
	for _, shell := range []string{"zsh", "bash"} {
		cmd := exec.Command(shell, "-l", "-c", "which node")
		if out, err := cmd.CombinedOutput(); err == nil {
			path := strings.TrimSpace(string(out))
			if path != "" && isExec(path) {
				return path, nil
			}
		}
	}

	// 2. Try exec.LookPath("node")
	if path, err := exec.LookPath("node"); err == nil {
		return path, nil
	}

	// 3. Fallback to NVM or other common paths
	if home, err := os.UserHomeDir(); err == nil {
		nvmDir := os.Getenv("NVM_DIR")
		if nvmDir == "" {
			nvmDir = filepath.Join(home, ".nvm")
		}
		versionsDir := filepath.Join(nvmDir, "versions", "node")
		if entries, err := os.ReadDir(versionsDir); err == nil {
			for _, entry := range entries {
				candidate := filepath.Join(versionsDir, entry.Name(), "bin", "node")
				if isExec(candidate) {
					return candidate, nil
				}
			}
		}
	}

	return "", fmt.Errorf("node not found")
}

func spawnClaude(opts spawnOptions) (*claudeProc, error) {
	claudeBin, err := resolveClaude()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "Using claude: %s\n", claudeBin)

	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-mode", "acceptEdits",
		"--verbose",
	}
	args = append(args, opts.extraArgs...)

	var cmd *exec.Cmd
	if isNodeScript(claudeBin) {
		nodeBin, err := resolveNode()
		if err == nil {
			fmt.Fprintf(os.Stderr, "Detected Node.js script. Spawning via node: %s\n", nodeBin)
			nodeArgs := append([]string{claudeBin}, args...)
			cmd = exec.Command(nodeBin, nodeArgs...)
		} else {
			fmt.Fprintf(os.Stderr, "Detected Node.js script but node not found: %v. Spawning directly.\n", err)
			cmd = exec.Command(claudeBin, args...)
		}
	} else {
		cmd = exec.Command(claudeBin, args...)
	}

	cmd.Dir = opts.projectDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	// Get stdin, stdout and stderr pipes
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdinPipe.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		stderrPipe.Close()
		stdoutPipe.Close()
		return nil, fmt.Errorf("cmd.Start: %w", err)
	}

	proc := &claudeProc{
		cmd:        cmd,
		stdinPipe:  stdinPipe,
		stderrPipe: stderrPipe,
		stdoutPipe: stdoutPipe,
		done:       make(chan struct{}),
	}

	// Start stdout translation goroutine
	go scanStdoutAndTranslate(proc, opts.eventsPath)

	return proc, nil
}

func scanStdoutAndTranslate(proc *claudeProc, eventsPath string) {
	scanner := bufio.NewScanner(proc.stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		translated, err := translateClaudeEvent(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[session] error translating event: %v (raw: %s)\n", err, string(line))
			_ = appendEventFile(eventsPath, line)
		} else if len(translated) > 0 {
			_ = appendEventFile(eventsPath, translated)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[session] stdout scanner error: %v\n", err)
	}
}

func translateClaudeEvent(line []byte) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(line, &base); err != nil {
		return nil, err
	}

	t, _ := base["type"].(string)
	st, _ := base["subtype"].(string)

	switch t {
	case "system":
		if st == "init" {
			sessionID, _ := base["session_id"].(string)
			cwd, _ := base["cwd"].(string)

			// Rewrite to session_start format
			type sessionStartData struct {
				SessionID string `json:"session_id"`
				Cwd       string `json:"cwd"`
			}
			type sessionStartEvent struct {
				Type    string           `json:"type"`
				Subtype string           `json:"subtype"`
				Data    sessionStartData `json:"data"`
			}
			ev := sessionStartEvent{
				Type:    "system",
				Subtype: "session_start",
				Data: sessionStartData{
					SessionID: sessionID,
					Cwd:       cwd,
				},
			}
			return json.Marshal(ev)
		}
	case "result":
		// Rewrite to session_end
		type sessionEndEvent struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		ev := sessionEndEvent{
			Type:    "system",
			Subtype: "session_end",
		}
		return json.Marshal(ev)
	}

	// Keep all other events as is
	return line, nil
}

func appendEventFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// appendInput appends one JSONL command to the claude input file.
func appendInput(inputPath string, v any) error {
	f, err := os.OpenFile(inputPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := fmt.Sprintf("%s\n", mustMarshal(v))
	_, err = fmt.Fprint(f, enc)
	return err
}
