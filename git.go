package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Git service ────────────────────────────────────────────────────────────
//
// A per-project Git panel: review the agent's changes as diffs, stage/commit
// them, and pull/push. Everything shells out to `git -C <projectPath> …` with
// no shell involved; paths are passed after `--` and validated in-repo via
// isSafePath, so a crafted filename can never become a git option. Network
// operations run with GIT_TERMINAL_PROMPT=0 so a missing credential fails fast
// instead of hanging on a prompt.

// gitLocks serialises write operations (stage/unstage/commit/pull/push) per
// project so concurrent actions never collide on .git/index.lock.
var (
	gitLocksMu sync.Mutex
	gitLocks   = map[string]*sync.Mutex{}
)

func gitLock(projectID string) *sync.Mutex {
	gitLocksMu.Lock()
	defer gitLocksMu.Unlock()
	m := gitLocks[projectID]
	if m == nil {
		m = &sync.Mutex{}
		gitLocks[projectID] = m
	}
	return m
}

// runGit runs `git -C dir args…` and returns stdout, stderr, error.
func runGit(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// gitErrMsg picks the most useful message out of a failed git invocation.
func gitErrMsg(out, errOut string, err error) string {
	m := strings.TrimSpace(errOut)
	if m == "" {
		m = strings.TrimSpace(out)
	}
	if m == "" && err != nil {
		m = err.Error()
	}
	return m
}

// isGitRepo reports whether dir is inside a git work tree.
func isGitRepo(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := runGit(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

// gitHasRemote reports whether at least one remote is configured.
func gitHasRemote(ctx context.Context, dir string) bool {
	out, _, err := runGit(ctx, dir, "remote")
	return err == nil && strings.TrimSpace(out) != ""
}

type gitFileEntry struct {
	Path string `json:"path"`
	Code string `json:"code"` // two-char porcelain XY code
}

// parseBranchHeader parses the "## …" line from `git status --branch`.
// Examples: "main...origin/main [ahead 1, behind 2]", "main", "HEAD (no branch)",
// "No commits yet on main".
func parseBranchHeader(h string) (branch, upstream string, ahead, behind int) {
	if strings.HasPrefix(h, "No commits yet on ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "No commits yet on ")), "", 0, 0
	}
	track := ""
	if idx := strings.Index(h, " ["); idx >= 0 {
		track = strings.TrimSuffix(h[idx+2:], "]")
		h = h[:idx]
	}
	if strings.Contains(h, "...") {
		parts := strings.SplitN(h, "...", 2)
		branch, upstream = parts[0], parts[1]
	} else {
		branch = h
	}
	for _, seg := range strings.Split(track, ", ") {
		seg = strings.TrimSpace(seg)
		if v := strings.TrimPrefix(seg, "ahead "); v != seg {
			ahead, _ = strconv.Atoi(v)
		} else if v := strings.TrimPrefix(seg, "behind "); v != seg {
			behind, _ = strconv.Atoi(v)
		}
	}
	return branch, upstream, ahead, behind
}

// isConflictCode reports whether an XY porcelain code marks an unmerged path.
func isConflictCode(x, y byte) bool {
	switch {
	case x == 'U' || y == 'U':
		return true
	case x == 'A' && y == 'A':
		return true
	case x == 'D' && y == 'D':
		return true
	}
	return false
}

type gitStatusResult struct {
	Branch, Upstream     string
	Ahead, Behind        int
	Staged, Unstaged     []gitFileEntry
	Untracked, Conflicts []gitFileEntry
}

// parsePorcelainStatus parses the NUL-separated output of
// `git status --porcelain -z --branch` into grouped file lists.
func parsePorcelainStatus(out string) gitStatusResult {
	res := gitStatusResult{
		Staged:    []gitFileEntry{},
		Unstaged:  []gitFileEntry{},
		Untracked: []gitFileEntry{},
		Conflicts: []gitFileEntry{},
	}
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		if strings.HasPrefix(rec, "## ") {
			res.Branch, res.Upstream, res.Ahead, res.Behind = parseBranchHeader(rec[3:])
			continue
		}
		if len(rec) < 3 {
			continue
		}
		x, y := rec[0], rec[1]
		path := rec[3:]
		// Rename/copy entries carry the original path in the following NUL field.
		if x == 'R' || x == 'C' {
			i++ // consume (and discard) the original path
		}
		code := string(rec[0:2])
		switch {
		case x == '?' && y == '?':
			res.Untracked = append(res.Untracked, gitFileEntry{Path: path, Code: code})
		case isConflictCode(x, y):
			res.Conflicts = append(res.Conflicts, gitFileEntry{Path: path, Code: code})
		default:
			if x != ' ' && x != '?' {
				res.Staged = append(res.Staged, gitFileEntry{Path: path, Code: code})
			}
			if y != ' ' && y != '?' {
				res.Unstaged = append(res.Unstaged, gitFileEntry{Path: path, Code: code})
			}
		}
	}
	return res
}

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isGitRepo(proj.Path) {
		writeJSON(w, map[string]any{"isRepo": false})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	out, errOut, err := runGit(ctx, proj.Path, "status", "--porcelain", "-z", "--branch")
	if err != nil {
		writeError(w, http.StatusInternalServerError, gitErrMsg(out, errOut, err))
		return
	}

	res := parsePorcelainStatus(out)
	writeJSON(w, map[string]any{
		"isRepo":    true,
		"branch":    res.Branch,
		"upstream":  res.Upstream,
		"ahead":     res.Ahead,
		"behind":    res.Behind,
		"hasRemote": gitHasRemote(ctx, proj.Path),
		"clean":     len(res.Staged)+len(res.Unstaged)+len(res.Untracked)+len(res.Conflicts) == 0,
		"staged":    res.Staged,
		"unstaged":  res.Unstaged,
		"untracked": res.Untracked,
		"conflicts": res.Conflicts,
	})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParam := r.URL.Query().Get("path")
	staged := r.URL.Query().Get("staged") == "true" || r.URL.Query().Get("staged") == "1"

	cleanPath := filepath.Clean("/" + pathParam)
	if pathParam != "" && !isSafePath(filepath.Join(proj.Path, cleanPath), proj.Path) {
		writeError(w, http.StatusForbidden, "invalid file path")
		return
	}
	rel := strings.TrimPrefix(cleanPath, "/")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var out string
	if staged {
		out, _, _ = runGit(ctx, proj.Path, "diff", "--staged", "--", rel)
	} else {
		out, _, _ = runGit(ctx, proj.Path, "diff", "--", rel)
		// An untracked file has no tracked diff; show it as an addition.
		if strings.TrimSpace(out) == "" && rel != "" {
			if o2, _, _ := runGit(ctx, proj.Path, "diff", "--no-index", "--", os.DevNull, rel); strings.TrimSpace(o2) != "" {
				out = o2
			}
		}
	}

	binary := strings.Contains(out, "Binary files ") || strings.Contains(out, "GIT binary patch")

	const maxDiff = 2 * 1024 * 1024
	truncated := false
	if len(out) > maxDiff {
		out = out[:maxDiff]
		truncated = true
	}

	writeJSON(w, map[string]any{
		"diff":      out,
		"binary":    binary,
		"truncated": truncated,
	})
}

// gitPathBody validates a POST body of {paths:[…]} against the repo and returns
// repo-relative paths, or writes an error and returns ok=false.
func (s *Server) gitPathBody(w http.ResponseWriter, r *http.Request, proj Project) ([]string, bool) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "paths is required")
		return nil, false
	}
	rels := make([]string, 0, len(body.Paths))
	for _, p := range body.Paths {
		cp := filepath.Clean("/" + p)
		if !isSafePath(filepath.Join(proj.Path, cp), proj.Path) {
			writeError(w, http.StatusForbidden, "invalid file path")
			return nil, false
		}
		rels = append(rels, strings.TrimPrefix(cp, "/"))
	}
	return rels, true
}

func (s *Server) handleGitStage(w http.ResponseWriter, r *http.Request, proj Project, unstage bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rels, ok := s.gitPathBody(w, r, proj)
	if !ok {
		return
	}

	lock := gitLock(proj.ID)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var args []string
	if unstage {
		args = append([]string{"reset", "-q", "HEAD", "--"}, rels...)
	} else {
		args = append([]string{"add", "--"}, rels...)
	}
	out, errOut, err := runGit(ctx, proj.Path, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, gitErrMsg(out, errOut, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "commit message is required")
		return
	}
	msg := strings.TrimSpace(body.Message)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	name, _, _ := runGit(ctx, proj.Path, "config", "user.name")
	email, _, _ := runGit(ctx, proj.Path, "config", "user.email")
	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		writeError(w, http.StatusBadRequest,
			"git identity not set — run: git config --global user.name \"Your Name\" && git config --global user.email \"you@example.com\"")
		return
	}

	lock := gitLock(proj.ID)
	lock.Lock()
	defer lock.Unlock()

	out, errOut, err := runGit(ctx, proj.Path, "commit", "-m", msg)
	if err != nil {
		// e.g. "nothing to commit" — surface git's own message.
		writeError(w, http.StatusBadRequest, gitErrMsg(out, errOut, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "output": strings.TrimSpace(out)})
}

func (s *Server) handleGitPull(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if !gitHasRemote(ctx, proj.Path) {
		writeError(w, http.StatusBadRequest, "no remote configured for this repository")
		return
	}

	lock := gitLock(proj.ID)
	lock.Lock()
	defer lock.Unlock()

	// --ff-only keeps this a safe operation: it never creates a merge commit or
	// leaves the tree in a conflicted state; a diverged branch fails cleanly.
	out, errOut, err := runGit(ctx, proj.Path, "pull", "--ff-only")
	combined := strings.TrimSpace(strings.TrimSpace(out) + "\n" + strings.TrimSpace(errOut))
	if err != nil {
		writeError(w, http.StatusConflict, gitErrMsg(out, errOut, err))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "output": combined})
}

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if !gitHasRemote(ctx, proj.Path) {
		writeError(w, http.StatusBadRequest, "no remote configured for this repository")
		return
	}

	lock := gitLock(proj.ID)
	lock.Lock()
	defer lock.Unlock()

	out, errOut, err := runGit(ctx, proj.Path, "push")
	combined := strings.TrimSpace(strings.TrimSpace(out) + "\n" + strings.TrimSpace(errOut))
	if err != nil {
		writeError(w, http.StatusConflict, gitErrMsg(out, errOut, err))
		return
	}
	if combined == "" {
		combined = "Everything up-to-date"
	}
	writeJSON(w, map[string]any{"ok": true, "output": combined})
}
