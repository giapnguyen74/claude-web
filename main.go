package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// cliOpts holds our own flags; everything else is forwarded to claude.
type cliOpts struct {
	workspace string   // defaults to cwd
	host      string   // defaults to "0.0.0.0"
	port      int      // defaults to 4000
	origins   []string // allowed WebSocket origins
}

type settingsFile struct {
	Workspace    string   `json:"workspace"`
	Host         string   `json:"host"`
	Port         *int     `json:"port"`
	Origins      []string `json:"origins"`
	PasswordHash string   `json:"passwordHash,omitempty"`
}

func loadSettings() (settingsFile, error) {
	var s settingsFile
	home, err := os.UserHomeDir()
	if err != nil {
		return s, err
	}
	path := filepath.Join(home, ".claude-code-web", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // file does not exist, that's fine
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// parseArgs splits os.Args into our flags and claude's flags.
// We claim: --workspace, --port, --host, --origins (and their -short forms).
// Everything else (e.g. -c, -y, --model) is forwarded to claude unchanged.
func parseArgs(base cliOpts) cliOpts {
	opts := base
	args := os.Args[1:]

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// --flag=value forms
		if v, ok := cutPrefix(arg, "--port="); ok {
			p, err := strconv.Atoi(v)
			if err != nil {
				fatalf("invalid port: %q", v)
			}
			opts.port = p
			continue
		}
		if v, ok := cutPrefix(arg, "--workspace="); ok {
			opts.workspace = v
			continue
		}
		if v, ok := cutPrefix(arg, "--host="); ok {
			opts.host = v
			continue
		}
		if v, ok := cutPrefix(arg, "--origins="); ok {
			opts.origins = strings.Split(v, ",")
			continue
		}
		if v, ok := cutPrefix(arg, "--origin="); ok {
			opts.origins = strings.Split(v, ",")
			continue
		}

		switch arg {
		case "--port", "-port":
			if i+1 < len(args) {
				i++
				p, err := strconv.Atoi(args[i])
				if err != nil {
					fatalf("invalid port: %q", args[i])
				}
				opts.port = p
			} else {
				fatalf("missing value for --port")
			}
		case "--workspace", "-workspace":
			if i+1 < len(args) {
				i++
				opts.workspace = args[i]
			} else {
				fatalf("missing value for --workspace")
			}
		case "--host", "-host":
			if i+1 < len(args) {
				i++
				opts.host = args[i]
			} else {
				fatalf("missing value for --host")
			}
		case "--origins", "-origins", "--origin", "-origin":
			if i+1 < len(args) {
				i++
				opts.origins = strings.Split(args[i], ",")
			} else {
				fatalf("missing value for --origins")
			}
		case "--help", "-h", "-help":
			printHelp()
			os.Exit(0)
		default:
			fatalf("unknown flag: %s", arg)
		}
	}

	return opts
}

// isLoopbackHost reports whether binding to host keeps the server reachable
// only from the local machine. "0.0.0.0" / "::" (all interfaces) and any LAN
// address are NOT loopback.
func isLoopbackHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func cutPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func printHelp() {
	fmt.Print(`Usage: claude-code-web [FLAGS]

Flags:
  --workspace <path>   Workspace directory containing projects (default: cwd)
  --port <n>           HTTP server port   (default: 4000)
  --host <addr>        Listen address     (default: 127.0.0.1 — loopback only)
  --origins <list>     WebSocket allowed origins (comma-separated list, e.g. "*")

Note: binding a non-loopback --host (e.g. 0.0.0.0) requires a password set
via 'claude-code-web --password', since the agent can run shell commands.

Examples:
  cd ~/workspace && claude-code-web
  claude-code-web --workspace ~/workspace
  claude-code-web --port 8080 --host 0.0.0.0`+"  # needs a password set"+`
`)
}

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--password" || os.Args[1] == "-password") {
		setupPassword()
		return
	}

	// 1. Start with hardcoded defaults. We bind loopback-only by default so the
	// (RCE-capable) agent is never exposed to the network without an explicit
	// opt-in via --host.
	baseOpts := cliOpts{
		host: "127.0.0.1",
		port: 4000,
	}

	// 2. Overwrite with ~/.claude-code-web/settings.json if present
	settings, err := loadSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load settings.json: %v\n", err)
	} else {
		if settings.Workspace != "" {
			baseOpts.workspace = settings.Workspace
		}
		if settings.Host != "" {
			baseOpts.host = settings.Host
		}
		if settings.Port != nil {
			baseOpts.port = *settings.Port
		}
		if settings.Origins != nil {
			baseOpts.origins = settings.Origins
		}
	}

	// 3. Overwrite with command-line arguments (highest priority)
	opts := parseArgs(baseOpts)

	// ── Resolve workspace directory ──────────────────────────────────────
	if opts.workspace == "" {
		opts.workspace, err = os.Getwd()
		if err != nil {
			fatalf("getwd: %v", err)
		}
	}
	workspace, err := filepath.Abs(opts.workspace)
	if err != nil {
		fatalf("resolving workspace: %v", err)
	}

	// Ensure workspace directory exists
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		fatalf("creating workspace dir: %v", err)
	}

	// ── Refuse unauthenticated network exposure ──────────────────────────
	// The agent can run arbitrary shell commands, so binding to a non-loopback
	// address without a password is effectively open RCE. Require a password
	// (set via `--password`) before listening on anything but loopback.
	if !isLoopbackHost(opts.host) && settings.PasswordHash == "" {
		fatalf("refusing to bind to non-loopback address %q without a password.\n"+
			"  The coding agent can execute shell commands, so exposing this UI\n"+
			"  unauthenticated gives anyone on the network arbitrary code execution.\n"+
			"  Set a password first:  claude-code-web --password\n"+
			"  Or bind to loopback:   claude-code-web --host 127.0.0.1", opts.host)
	}

	// ── Initialize project store ─────────────────────────────────────────
	projects, err := NewProjectStore(workspace)
	if err != nil {
		fatalf("project store: %v", err)
	}

	// ── Initialize process manager ───────────────────────────────────────
	procmgr := NewProcManager()

	// ── Print startup info ───────────────────────────────────────────────
	displayHost := opts.host
	if displayHost == "0.0.0.0" || displayHost == "127.0.0.1" || displayHost == "::" {
		displayHost = "localhost"
	}
	url := fmt.Sprintf("http://%s:%d", displayHost, opts.port)
	fmt.Fprintf(os.Stderr, "\n  Web UI   → %s\n", url)
	fmt.Fprintf(os.Stderr, "  Workspace → %s\n\n", workspace)

	// ── Start HTTP server ────────────────────────────────────────────────
	home, _ := os.UserHomeDir()
	srv := newServer(serverConfig{
		host:         opts.host,
		port:         opts.port,
		origins:      opts.origins,
		workspace:    workspace,
		passwordHash: settings.PasswordHash,
		configDir:    filepath.Join(home, ".claude-code-web"),
	}, projects, procmgr)

	go func() {
		if err := srv.run(); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			procmgr.KillAll()
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown on SIGTERM/SIGINT ───────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	fmt.Fprintln(os.Stderr, "\nReceived signal, shutting down...")
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "HTTP Server shutdown error: %v\n", err)
	}

	procmgr.KillAll()
	os.Exit(0)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func setupPassword() {
	fmt.Print("Enter new password: ")
	pwdBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		fatalf("failed to read password: %v", err)
	}
	fmt.Println()

	fmt.Print("Confirm password: ")
	confirmBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		fatalf("failed to read password: %v", err)
	}
	fmt.Println()

	if string(pwdBytes) != string(confirmBytes) {
		fatalf("passwords do not match")
	}

	hash, err := bcrypt.GenerateFromPassword(pwdBytes, bcrypt.DefaultCost)
	if err != nil {
		fatalf("failed to hash password: %v", err)
	}

	settings, _ := loadSettings()
	settings.PasswordHash = string(hash)

	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("user home: %v", err)
	}
	path := filepath.Join(home, ".claude-code-web", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		fatalf("mkdir: %v", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fatalf("marshal: %v", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		fatalf("write settings: %v", err)
	}

	fmt.Println("Password set successfully.")
}
