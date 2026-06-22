package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── Project model ─────────────────────────────────────────────────────────

// Project represents a registered project in the workspace.
type Project struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	ClaudeArgs []string `json:"claudeArgs,omitempty"`
	QwenArgs   []string `json:"qwenArgs,omitempty"` // For backward compatibility
	CreatedAt  string   `json:"createdAt"`
}

type projectsFile struct {
	Projects         []Project `json:"projects"`
	GlobalClaudeArgs []string  `json:"globalClaudeArgs,omitempty"`
	GlobalQwenArgs   []string  `json:"globalQwenArgs,omitempty"` // For backward compatibility
}

// ── ProjectStore ──────────────────────────────────────────────────────────

// ProjectStore manages the list of projects with thread-safe access
// and persists changes to ~/.claude-code-web/projects.json.
type ProjectStore struct {
	mu               sync.RWMutex
	projects         []Project
	globalClaudeArgs []string
	filePath         string
	workspace        string // absolute path to workspace root
}

// NewProjectStore creates a store bound to the given workspace directory.
func NewProjectStore(workspace string) (*ProjectStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}
	fp := filepath.Join(home, ".claude-code-web", "projects.json")
	store := &ProjectStore{
		filePath:  fp,
		workspace: workspace,
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// ── Persistence ───────────────────────────────────────────────────────────

func (s *ProjectStore) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.projects = []Project{}
			return nil
		}
		return fmt.Errorf("read projects.json: %w", err)
	}
	var pf projectsFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return fmt.Errorf("parse projects.json: %w", err)
	}
	s.projects = pf.Projects
	if s.projects == nil {
		s.projects = []Project{}
	}
	for i, p := range s.projects {
		if len(p.ClaudeArgs) == 0 && len(p.QwenArgs) > 0 {
			s.projects[i].ClaudeArgs = p.QwenArgs
		}
	}
	s.globalClaudeArgs = pf.GlobalClaudeArgs
	if len(s.globalClaudeArgs) == 0 && len(pf.GlobalQwenArgs) > 0 {
		s.globalClaudeArgs = pf.GlobalQwenArgs
	}
	return nil
}

func (s *ProjectStore) save() error {
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(projectsFile{
		Projects:         s.projects,
		GlobalClaudeArgs: s.globalClaudeArgs,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o644)
}

// ── ID generation ─────────────────────────────────────────────────────────

func projectID(absPath string) string {
	base := filepath.Base(absPath)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(absPath)))[:8]
	return fmt.Sprintf("%s_%s", base, hash)
}

// ── Read operations ───────────────────────────────────────────────────────

// ListProjects returns a copy of all projects.
func (s *ProjectStore) ListProjects() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, len(s.projects))
	copy(out, s.projects)
	return out
}

// GetProject returns a project by ID, or nil if not found.
func (s *ProjectStore) GetProject(id string) *Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.projects {
		if p.ID == id {
			cp := p
			return &cp
		}
	}
	return nil
}

// GetWorkspace returns the workspace root path.
func (s *ProjectStore) GetWorkspace() string {
	return s.workspace
}

// GetGlobalClaudeArgs returns the global claude arguments.
func (s *ProjectStore) GetGlobalClaudeArgs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.globalClaudeArgs))
	copy(out, s.globalClaudeArgs)
	return out
}

// SetGlobalClaudeArgs updates the global claude arguments.
func (s *ProjectStore) SetGlobalClaudeArgs(args []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalClaudeArgs = args
	return s.save()
}

// ── Write operations ──────────────────────────────────────────────────────

// AddExistingFolder registers an existing directory as a project.
// The path must exist and be inside the workspace.
func (s *ProjectStore) AddExistingFolder(folderPath string) (*Project, error) {
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	// Validate: must exist
	fi, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}

	// Validate: must be inside workspace
	if !isUnderDir(absPath, s.workspace) {
		return nil, fmt.Errorf("folder %s is not inside workspace %s", absPath, s.workspace)
	}

	id := projectID(absPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check duplicate
	for _, p := range s.projects {
		if p.ID == id {
			return nil, fmt.Errorf("project already registered: %s", p.Name)
		}
	}

	proj := Project{
		ID:        id,
		Name:      filepath.Base(absPath),
		Path:      absPath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.projects = append(s.projects, proj)
	if err := s.save(); err != nil {
		// Rollback
		s.projects = s.projects[:len(s.projects)-1]
		return nil, fmt.Errorf("save: %w", err)
	}
	return &proj, nil
}

// CreateNewRepo creates a new directory with git init inside the workspace.
func (s *ProjectStore) CreateNewRepo(name string) (*Project, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	// Sanitize name
	name = strings.TrimSpace(name)
	if strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("name must not contain path separators")
	}

	absPath := filepath.Join(s.workspace, name)

	// Check if already exists
	if _, err := os.Stat(absPath); err == nil {
		return nil, fmt.Errorf("directory already exists: %s", absPath)
	}

	// Create directory
	if err := os.MkdirAll(absPath, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// Git init
	cmd := exec.Command("git", "-C", absPath, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "[projects] git init warning: %s\n", strings.TrimSpace(string(out)))
	}

	// Write .gitignore
	gi := filepath.Join(absPath, ".gitignore")
	os.WriteFile(gi, []byte("node_modules/\ndist/\n.claude-code-web/\n*.log\n"), 0o644) //nolint:errcheck

	// Initial commit to avoid 'branch does not have any commits yet' fatal error
	exec.Command("git", "-C", absPath, "add", ".gitignore").Run() //nolint:errcheck
	exec.Command("git", "-C", absPath, "commit", "-m", "Initial commit").Run() //nolint:errcheck

	id := projectID(absPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	proj := Project{
		ID:        id,
		Name:      name,
		Path:      absPath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.projects = append(s.projects, proj)
	if err := s.save(); err != nil {
		s.projects = s.projects[:len(s.projects)-1]
		return nil, fmt.Errorf("save: %w", err)
	}
	return &proj, nil
}

// CloneRepo clones a git repository into the workspace.
// Supports HTTPS and SSH URLs.
func (s *ProjectStore) CloneRepo(gitURL, name string) (*Project, error) {
	if gitURL == "" {
		return nil, fmt.Errorf("git URL is required")
	}

	// Infer name from URL if not provided
	if name == "" {
		name = inferRepoName(gitURL)
	}
	if name == "" {
		return nil, fmt.Errorf("could not infer repo name from URL; please provide a name")
	}

	absPath := filepath.Join(s.workspace, name)

	// Check if already exists
	if _, err := os.Stat(absPath); err == nil {
		return nil, fmt.Errorf("directory already exists: %s", absPath)
	}

	// Git clone
	cmd := exec.Command("git", "clone", gitURL, absPath)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(out)))
	}

	id := projectID(absPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	proj := Project{
		ID:        id,
		Name:      name,
		Path:      absPath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.projects = append(s.projects, proj)
	if err := s.save(); err != nil {
		s.projects = s.projects[:len(s.projects)-1]
		return nil, fmt.Errorf("save: %w", err)
	}
	return &proj, nil
}

// UpdateProject updates a project's mutable fields (name, claudeArgs).
func (s *ProjectStore) UpdateProject(id string, name string, claudeArgs []string) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, p := range s.projects {
		if p.ID == id {
			if name != "" {
				s.projects[i].Name = name
			}
			s.projects[i].ClaudeArgs = claudeArgs
			if err := s.save(); err != nil {
				return nil, fmt.Errorf("save: %w", err)
			}
			cp := s.projects[i]
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("project not found: %s", id)
}

// RemoveProject removes a project from the registry.
// It does NOT delete the actual folder on disk.
func (s *ProjectStore) RemoveProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, p := range s.projects {
		if p.ID == id {
			s.projects = append(s.projects[:i], s.projects[i+1:]...)
			return s.save()
		}
		_ = p
	}
	return fmt.Errorf("project not found: %s", id)
}

// ── Helpers ───────────────────────────────────────────────────────────────

// isUnderDir checks if child is under (or equal to) parent directory.
func isUnderDir(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// inferRepoName extracts the repo name from a git URL.
// e.g. "https://github.com/user/repo.git" → "repo"
//
//	"git@github.com:user/repo.git" → "repo"
func inferRepoName(gitURL string) string {
	// Remove trailing .git
	u := strings.TrimSuffix(gitURL, ".git")
	// Remove trailing slash
	u = strings.TrimRight(u, "/")

	// SSH format: git@host:user/repo
	if idx := strings.LastIndex(u, ":"); idx > 0 && !strings.Contains(u, "://") {
		u = u[idx+1:]
	}

	// Take last path component
	if idx := strings.LastIndex(u, "/"); idx >= 0 {
		return u[idx+1:]
	}
	return u
}
