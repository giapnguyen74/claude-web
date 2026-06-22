package main

import (
	"embed"
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

//go:embed public/dashboard.html
var dashboardHTML []byte

//go:embed public/index.html
var conversationHTML []byte

//go:embed public/*
var publicFS embed.FS

// ── WebSocket hub ─────────────────────────────────────────────────────────

type hub struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{subscribers: make(map[chan []byte]struct{})}
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 512)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
}

func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- msg:
		default: // slow client — skip rather than block
		}
	}
}

// ── Server ────────────────────────────────────────────────────────────────

type serverConfig struct {
	host         string
	port         int
	origins      []string
	workspace    string
	passwordHash string
	configDir    string
}

type inputJob struct {
	data   any
	respCh chan error
}

type Server struct {
	cfg      serverConfig
	projects *ProjectStore
	procmgr  *ProcManager
	upgrader websocket.Upgrader
	srv      *http.Server

	tokensMu sync.Mutex
	tokens   map[string]time.Time
}

func (s *Server) loadTokens() {
	b, err := os.ReadFile(filepath.Join(s.cfg.configDir, "tokens.json"))
	if err == nil {
		s.tokensMu.Lock()
		json.Unmarshal(b, &s.tokens)
		s.tokensMu.Unlock()
	}
}

func (s *Server) saveTokens() {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	b, err := json.Marshal(s.tokens)
	if err == nil {
		os.WriteFile(filepath.Join(s.cfg.configDir, "tokens.json"), b, 0600)
	}
}

func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		host = h
	}
	// Strip square brackets from IPv6 host if present
	hostClean := strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || hostClean == "::1"
}

func checkOrigin(origin string, allowedOrigins []string) bool {
	if origin == "" {
		return true
	}

	// Local loopback is ALWAYS allowed (localhost, 127.0.0.1, [::1])
	if isLocalOrigin(origin) {
		return true
	}

	// If it is a remote origin, it MUST match one of the explicitly allowed origins
	if len(allowedOrigins) > 0 {
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		host := u.Host
		if h, _, err := net.SplitHostPort(u.Host); err == nil {
			host = h
		}
		for _, allowed := range allowedOrigins {
			cleanAllowed := strings.TrimRight(allowed, "/")
			if strings.EqualFold(origin, cleanAllowed) {
				return true
			}
			if strings.EqualFold(u.Host, cleanAllowed) {
				return true
			}
			if strings.EqualFold(host, cleanAllowed) {
				return true
			}
		}
	}

	return false
}

func newServer(cfg serverConfig, projects *ProjectStore, procmgr *ProcManager) *Server {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			ok := checkOrigin(origin, cfg.origins)
			if !ok {
				fmt.Printf("[ws] origin rejected: %s (allowed: %v)\n", origin, cfg.origins)
			}
			return ok
		},
	}

	s := &Server{
		cfg:      cfg,
		projects: projects,
		procmgr:  procmgr,
		upgrader: upgrader,
		tokens:   make(map[string]time.Time),
	}
	s.loadTokens()
	return s
}

func (s *Server) run() error {
	mux := http.NewServeMux()

	// ── Page routes ───────────────────────────────────────────────────
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/project/", s.handleConversationPage)
	mux.Handle("/public/", http.FileServer(http.FS(publicFS)))

	mux.HandleFunc("/api/auth/login", s.handleLogin)

	// Auth wrapper for all API routes (excluding login itself)
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/projects", s.handleListProjects)
	apiMux.HandleFunc("/api/projects/add-folder", s.handleAddFolder)
	apiMux.HandleFunc("/api/projects/create-repo", s.handleCreateRepo)
	apiMux.HandleFunc("/api/projects/clone", s.handleCloneRepo)
	apiMux.HandleFunc("/api/settings/global", s.handleUpdateGlobalSettings)
	apiMux.HandleFunc("/api/workspace/folders", s.handleWorkspaceFolders)
	apiMux.HandleFunc("/api/check-origin", s.handleCheckOrigin)
	apiMux.HandleFunc("/api/projects/", s.handleProjectAPI)

	mux.Handle("/api/", s.authMiddleware(apiMux))

	addr := fmt.Sprintf("%s:%d", s.cfg.host, s.cfg.port)
	s.srv = &http.Server{Addr: addr, Handler: mux}

	fmt.Fprintf(os.Stderr, "  [server] HTTP Server starting to listen on %s...\n", addr)
	err := s.srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "  [server] HTTP Server stopped: %v\n", err)
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleProjectFiles(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dirParam := r.URL.Query().Get("dir")
	// Clean the path to prevent directory traversal
	cleanDir := filepath.Clean("/" + dirParam)

	// absolute target directory
	targetDir := filepath.Join(proj.Path, cleanDir)

	// Ensure the targetDir is inside the project path (sanity check after Clean)
	if !isSafePath(targetDir, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid directory")
		return
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type fileEntry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
	}

	var list []fileEntry
	for _, e := range entries {
		list = append(list, fileEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
		})
	}

	writeJSON(w, map[string]any{
		"path":  cleanDir,
		"files": list,
	})
}

func (s *Server) handleProjectRawFile(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParam := r.URL.Query().Get("path")
	cleanPath := filepath.Clean("/" + pathParam)
	targetPath := filepath.Join(proj.Path, cleanPath)

	if !isSafePath(targetPath, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid file path")
		return
	}

	http.ServeFile(w, r, targetPath)
}

func (s *Server) handleProjectReadFile(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParam := r.URL.Query().Get("path")
	cleanPath := filepath.Clean("/" + pathParam)
	targetPath := filepath.Join(proj.Path, cleanPath)

	if !isSafePath(targetPath, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid file path")
		return
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	}

	const maxTextSize = 2 * 1024 * 1024 // 2MB
	if info.Size() > maxTextSize {
		// Just to check if it's media before flatly rejecting
		ext := strings.ToLower(filepath.Ext(targetPath))
		isMediaExt := ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp" || ext == ".mp4" || ext == ".webm"
		if !isMediaExt {
			writeError(w, http.StatusBadRequest, "file too large for inline viewing (max 2MB)")
			return
		}
	}

	f, err := os.Open(targetPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	// Read first 512 bytes for sniffing
	head := make([]byte, 512)
	n, _ := f.Read(head)
	head = head[:n]

	contentType := http.DetectContentType(head)
	isText := strings.HasPrefix(contentType, "text/") || contentType == "application/json"
	
	// Better fallback for source code files which sometimes detect as application/octet-stream
	if !isText {
		if !bytes.Contains(head, []byte{0}) {
			isText = true
		}
	}

	if isText {
		if info.Size() > maxTextSize {
			writeError(w, http.StatusBadRequest, "text file too large for inline viewing (max 2MB)")
			return
		}
		// Read entire file
		f.Seek(0, 0)
		data, err := os.ReadFile(targetPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"type":    "text",
			"content": string(data),
		})
		return
	}

	if strings.HasPrefix(contentType, "image/") {
		writeJSON(w, map[string]any{
			"type": "image",
			"url":  fmt.Sprintf("/api/projects/%s/files/raw?path=%s", proj.ID, url.QueryEscape(pathParam)),
		})
		return
	}

	if strings.HasPrefix(contentType, "video/") {
		writeJSON(w, map[string]any{
			"type": "video",
			"url":  fmt.Sprintf("/api/projects/%s/files/raw?path=%s", proj.ID, url.QueryEscape(pathParam)),
		})
		return
	}

	writeError(w, http.StatusBadRequest, "binary file format not supported for inline viewing")
}

func (s *Server) handleProjectDownloadFile(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParam := r.URL.Query().Get("path")
	cleanPath := filepath.Clean("/" + pathParam)
	targetPath := filepath.Join(proj.Path, cleanPath)

	if !isSafePath(targetPath, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid file path")
		return
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	basename := filepath.Base(targetPath)
	if basename == "/" || basename == "." {
		basename = filepath.Base(proj.Path)
	}

	if info.IsDir() {
		// Serve as ZIP
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, basename))

		zw := zip.NewWriter(w)
		defer zw.Close()

		filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			relPath, err := filepath.Rel(filepath.Dir(targetPath), path)
			if err != nil {
				return err
			}

			if info.IsDir() {
				if !strings.HasSuffix(relPath, "/") {
					relPath += "/"
				}
				_, err = zw.Create(relPath)
				return err
			}

			f, err := os.Open(path)
			if err != nil {
				return nil // Skip files we can't read
			}
			defer f.Close()

			fw, err := zw.Create(relPath)
			if err != nil {
				return err
			}

			_, err = io.Copy(fw, f)
			return err
		})
	} else {
		// Serve file
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, basename))
		f, err := os.Open(targetPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to open file")
			return
		}
		defer f.Close()
		io.Copy(w, f)
	}
}

// ── Project action handlers ─────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML) //nolint:errcheck
}

func (s *Server) handleConversationPage(w http.ResponseWriter, r *http.Request) {
	// Extract project ID from /project/{id}
	id := strings.TrimPrefix(r.URL.Path, "/project/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	// Verify project exists
	proj := s.projects.GetProject(id)
	if proj == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(conversationHTML) //nolint:errcheck
}

func (s *Server) handleCheckOrigin(w http.ResponseWriter, r *http.Request) {
	origin := r.URL.Query().Get("origin")
	if !checkOrigin(origin, s.cfg.origins) {
		writeError(w, http.StatusForbidden, "invalid origin")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ── Project CRUD endpoints ────────────────────────────────────────────────

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	projects := s.projects.ListProjects()
	active := s.procmgr.ListActive()

	type projectInfo struct {
		Project
		Status    string `json:"status"`
		SessionID string `json:"sessionId,omitempty"`
	}

	result := make([]projectInfo, len(projects))
	for i, p := range projects {
		info := projectInfo{Project: p, Status: "idle"}
		if ap, ok := active[p.ID]; ok {
			status, sessionID := ap.State.get()
			info.Status = status
			info.SessionID = sessionID
		}
		result[i] = info
	}

	writeJSON(w, map[string]any{
		"workspace":        s.projects.GetWorkspace(),
		"globalClaudeArgs": s.projects.GetGlobalClaudeArgs(),
		"projects":         result,
	})
}

func (s *Server) handleWorkspaceFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ws := s.projects.GetWorkspace()
	entries, err := os.ReadDir(ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var folders []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			folders = append(folders, e.Name())
		}
	}
	writeJSON(w, map[string]any{"folders": folders})
}

// ── Auth ──────────────────────────────────────────────────────────────────

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.passwordHash == "" {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie("claude_auth")
		if err != nil || cookie.Value == "" {
			fmt.Printf("[auth] unauthorized: missing or empty claude_auth cookie (path: %s)\n", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		s.tokensMu.Lock()
		expiry, ok := s.tokens[cookie.Value]
		isValid := ok && time.Now().Before(expiry)
		if isValid {
			s.tokens[cookie.Value] = time.Now().Add(10 * 24 * time.Hour)
		}
		s.tokensMu.Unlock()

		if !isValid {
			fmt.Printf("[auth] unauthorized: token expired or invalid (path: %s)\n", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Renew the cookie in the browser to match the server-side extension
		http.SetCookie(w, &http.Cookie{
			Name:     "claude_auth",
			Value:    cookie.Value,
			Path:     "/",
			MaxAge:   864000, // 10 days
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.cfg.passwordHash == "" {
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	err := bcrypt.CompareHashAndPassword([]byte(s.cfg.passwordHash), []byte(body.Password))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	// Generate a secure token
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)

	s.tokensMu.Lock()
	s.tokens[token] = time.Now().Add(10 * 24 * time.Hour)
	s.tokensMu.Unlock()
	s.saveTokens()

	http.SetCookie(w, &http.Cookie{
		Name:     "claude_auth",
		Value:    token,
		Path:     "/",
		MaxAge:   864000, // 10 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleUpdateGlobalSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		GlobalClaudeArgs []string `json:"globalClaudeArgs"`
		GlobalQwenArgs   []string `json:"globalQwenArgs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	args := body.GlobalClaudeArgs
	if len(args) == 0 {
		args = body.GlobalQwenArgs
	}
	if err := s.projects.SetGlobalClaudeArgs(args); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAddFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	proj, err := s.projects.AddExistingFolder(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, proj)
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	proj, err := s.projects.CreateNewRepo(body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, proj)
}

func (s *Server) handleCloneRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	proj, err := s.projects.CloneRepo(body.URL, body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, proj)
}

// ── Project-scoped API router ─────────────────────────────────────────────

func (s *Server) handleProjectAPI(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/projects/{id}/{action}
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	projectID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// DELETE /api/projects/{id} — remove project
	if action == "" && r.Method == http.MethodDelete {
		// Stop if running
		if ap := s.procmgr.GetActive(projectID); ap != nil {
			_ = s.procmgr.Stop(projectID)
		}
		if err := s.projects.RemoveProject(projectID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	// PUT /api/projects/{id} — update project config
	if action == "" && r.Method == http.MethodPut {
		var body struct {
			Name       string   `json:"name"`
			ClaudeArgs []string `json:"claudeArgs"`
			QwenArgs   []string `json:"qwenArgs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		args := body.ClaudeArgs
		if len(args) == 0 {
			args = body.QwenArgs
		}
		proj, err := s.projects.UpdateProject(projectID, body.Name, args)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, proj)
		return
	}

	// Verify project exists for action routes
	proj := s.projects.GetProject(projectID)
	if proj == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	switch action {
	case "start":
		s.handleProjectStart(w, r, *proj)
	case "stop":
		s.handleProjectStop(w, r, projectID)
	case "status":
		s.handleProjectStatus(w, r, projectID)
	case "message":
		s.handleProjectMessage(w, r, projectID)
	case "approve":
		s.handleProjectApprove(w, r, projectID)
	case "events":
		s.handleProjectWS(w, r, projectID)
	case "events/history":
		s.handleProjectEventsHistory(w, r, *proj)
	case "files":
		s.handleProjectFiles(w, r, *proj)
	case "files/read":
		s.handleProjectReadFile(w, r, *proj)
	case "files/raw":
		s.handleProjectRawFile(w, r, *proj)
	case "files/download":
		s.handleProjectDownloadFile(w, r, *proj)
	case "files/upload":
		s.handleProjectUploadFile(w, r, *proj)
	case "files/delete":
		s.handleProjectDeleteFile(w, r, *proj)
	default:
		http.NotFound(w, r)
	}
}

// ── Project action handlers ───────────────────────────────────────────────

func (s *Server) handleProjectDeleteFile(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParam := r.URL.Query().Get("path")
	cleanPath := filepath.Clean("/" + pathParam)
	targetPath := filepath.Join(proj.Path, cleanPath)

	if !isSafePath(targetPath, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid file path")
		return
	}

	// Do not allow deleting the project root
	if filepath.Clean(targetPath) == filepath.Clean(proj.Path) {
		writeError(w, http.StatusForbidden, "cannot delete project root")
		return
	}

	if err := os.RemoveAll(targetPath); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete: "+err.Error())
		return
	}

	writeJSON(w, map[string]string{"status": "deleted", "path": targetPath})
}

func (s *Server) handleProjectUploadFile(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max in memory
		writeError(w, http.StatusBadRequest, "failed to parse multipart form")
		return
	}

	pathParam := r.URL.Query().Get("path")
	cleanPath := filepath.Clean("/" + pathParam)
	targetDir := filepath.Join(proj.Path, cleanPath)

	if !isSafePath(targetDir, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid directory path")
		return
	}

	info, err := os.Stat(targetDir)
	if err != nil {
		writeError(w, http.StatusNotFound, "directory not found")
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "target path is not a directory")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' in form")
		return
	}
	defer file.Close()

	destPath := filepath.Join(targetDir, filepath.Base(header.Filename))
	if !isSafePath(destPath, proj.Path) {
		writeError(w, http.StatusForbidden, "invalid destination path")
		return
	}

	dst, err := os.Create(destPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create destination file")
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save file")
		return
	}

	writeJSON(w, map[string]string{"status": "ok", "path": destPath})
}

func (s *Server) handleProjectStart(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	claudeArgs := s.projects.GetGlobalClaudeArgs()
	if len(proj.ClaudeArgs) > 0 {
		claudeArgs = proj.ClaudeArgs
	}

	ap, err := s.procmgr.Start(proj, claudeArgs)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	status, sessionID := ap.State.get()
	writeJSON(w, map[string]any{
		"ok":        true,
		"status":    status,
		"sessionId": sessionID,
	})
}

func (s *Server) handleProjectStop(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.procmgr.Stop(projectID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleProjectStatus(w http.ResponseWriter, r *http.Request, projectID string) {
	proj := s.projects.GetProject(projectID)
	if proj == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	status := "idle"
	sessionID := ""
	if ap := s.procmgr.GetActive(projectID); ap != nil {
		status, sessionID = ap.State.get()
	}

	writeJSON(w, map[string]any{
		"status":     status,
		"sessionId":  sessionID,
		"projectId":  proj.ID,
		"projectDir": proj.Path,
		"name":       proj.Name,
	})
}

func (s *Server) handleProjectMessage(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	ap := s.procmgr.GetActive(projectID)
	if ap == nil {
		writeError(w, http.StatusConflict, "project is not running")
		return
	}
	status, _ := ap.State.get()
	if status == "stopped" {
		writeError(w, http.StatusConflict, "session is stopped")
		return
	}

	if err := ap.sendInput(map[string]any{
		"type": "submit",
		"text": strings.TrimSpace(body.Text),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleProjectApprove(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		RequestID string `json:"requestId"`
		Allowed   *bool  `json:"allowed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID == "" || body.Allowed == nil {
		writeError(w, http.StatusBadRequest, "requestId and allowed are required")
		return
	}

	ap := s.procmgr.GetActive(projectID)
	if ap == nil {
		writeError(w, http.StatusConflict, "project is not running")
		return
	}

	if err := ap.sendInput(map[string]any{
		"type":       "confirmation_response",
		"request_id": body.RequestID,
		"allowed":    *body.Allowed,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// replayLimit bounds how many of the most recent events are sent on WS connect
// (and per history page). Older events load lazily as the user scrolls up.
const replayLimit = 300

// handleProjectEventsHistory returns an older page of events for scroll-back.
// Query: before=<index> (exclusive upper bound; default = end), limit=<n>.
func (s *Server) handleProjectEventsHistory(w http.ResponseWriter, r *http.Request, proj Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	evPath, err := eventsPathForProject(proj.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	histPath, err := historyPathForProject(proj.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Combined timeline: archived prior sessions, then the current session.
	all := append(readJSONLines(histPath), readJSONLines(evPath)...)
	total := len(all)

	limit := replayLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}

	end := total
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < end {
			end = n
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}

	writeJSON(w, map[string]any{
		"events":     all[start:end],
		"firstIndex": start,
		"hasMore":    start > 0,
	})
}

// ── WebSocket handler (per-project event stream) ──────────────────────────

func (s *Server) handleProjectWS(w http.ResponseWriter, r *http.Request, projectID string) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ap := s.procmgr.GetActive(projectID)

	// Send initial status
	proj := s.projects.GetProject(projectID)
	if proj == nil {
		return
	}

	status := "idle"
	sessionID := ""
	if ap != nil {
		status, sessionID = ap.State.get()
	}

	write := func(msg []byte) bool {
		return conn.WriteMessage(websocket.TextMessage, msg) == nil
	}

	// 1. Current status
	if !write(mustMarshal(map[string]any{
		"type":      "server_status",
		"status":    status,
		"sessionId": sessionID,
	})) {
		return
	}

	// Subscribe to live events BEFORE replay (active projects only) so nothing
	// emitted during replay is missed. Idle projects have no live channel but
	// still get their history replayed below.
	var liveCh chan []byte
	if ap != nil {
		liveCh = ap.Hub.subscribe()
		defer ap.Hub.unsubscribe(liveCh)
	}

	// 2. Replay a bounded tail of the CURRENT session only, so the live channel
	//    never replays a stale session_end from a prior run. firstIndex is the
	//    position in the combined (archive + current) timeline, so scroll-up
	//    flows seamlessly into history via GET .../events/history.
	evPath, _ := eventsPathForProject(proj.Path)
	histPath, _ := historyPathForProject(proj.Path)
	cur := readJSONLines(evPath)
	archivedCount := len(readJSONLines(histPath))
	total := len(cur)
	start := total - replayLimit
	if start < 0 {
		start = 0
	}
	firstIndex := archivedCount + start
	if !write(mustMarshal(map[string]any{
		"type":       "replay_start",
		"firstIndex": firstIndex,
		"hasMore":    firstIndex > 0,
	})) {
		return
	}
	for _, ev := range cur[start:] {
		if !write(marshalClaudeEvent(ev)) {
			return
		}
	}
	// 3. Flush events buffered during replay (no gap) — active projects only.
	if liveCh != nil {
	flush:
		for {
			select {
			case ev := <-liveCh:
				if !write(ev) {
					return
				}
			default:
				break flush
			}
		}
	}
	if !write([]byte(`{"type":"replay_end"}`)) {
		return
	}

	// 4. Drain incoming WS frames
	connClosed := make(chan struct{})
	go func() {
		defer close(connClosed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// 5. Stream live events until the client disconnects. Idle projects have no
	//    live channel, so just hold the connection open until it closes.
	if liveCh == nil {
		<-connClosed
		return
	}
	for {
		select {
		case ev, ok := <-liveCh:
			if !ok {
				return
			}
			if !write(ev) {
				return
			}
		case <-connClosed:
			return
		}
	}
}

// ── Event processing helpers (used by ProcManager) ────────────────────────

// onProjectLiveEvent processes a raw event for an active project:
// updates state and broadcasts to all WS clients.
func onProjectLiveEvent(ap *ActiveProject, raw json.RawMessage) {
	var base struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Data    struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &base); err == nil {
		if base.Type == "system" {
			switch base.Subtype {
			case "session_start":
				if base.Data.SessionID != "" {
					ap.State.setRunning(base.Data.SessionID)
					ap.Hub.broadcast(marshalProjectStatus(ap))
				}
			case "session_end":
				ap.State.setStopped()
				ap.Hub.broadcast(marshalProjectStatus(ap))
			}
		}
	}
	ap.Hub.broadcast(marshalClaudeEvent(raw))
}

// marshalProjectStatus creates a server_status WS message for a project.
func marshalProjectStatus(ap *ActiveProject) []byte {
	status, sessionID := ap.State.get()
	return mustMarshal(map[string]any{
		"type":      "server_status",
		"status":    status,
		"sessionId": sessionID,
	})
}

// marshalClaudeEvent wraps a raw claude event for WS transmission.
func marshalClaudeEvent(raw json.RawMessage) []byte {
	return mustMarshal(map[string]any{
		"type": "claude_event",
		"data": raw,
	})
}

// ── JSON utilities ────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg}) //nolint:errcheck
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func isSafePath(target, base string) bool {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		realBase = base
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		realTarget = target
	}
	rel, err := filepath.Rel(realBase, realTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ".."
}
