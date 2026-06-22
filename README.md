# claude-code-web

> **The Problem:** 
> - We don't want to SSH into our dev computers just to vibe code. We want to sit on the beach with a lightweight device to monitor and prompt our projects.
> - We don't want heavy, bloated tools. We want a simple, lightweight interface that we can prompt, go grab a coffee, and check the results later.
> - We want to work securely on our phones without going through the hassle of setting up complex third-party messaging channels.

`claude-code-web` solves this by giving you a unified, mobile-friendly web dashboard to manage multiple Claude Code sessions working across different projects simultaneously. 

It is a lightweight, single-binary solution that lets you leave your AI agents running 24/7. You can check in on their progress, review their work, and prompt them for the next features from anywhere. It brings highly flexible "vibe coding" to any device you own—without the bloat.

### Gallery

<p align="center">
  <img src="assets/Screenshot1.png" width="800" alt="Desktop Dashboard">
</p>


<p align="center">
  <img src="assets/mobile-demo.webp" width="300" alt="Mobile Vibe Coding">
</p>

## Features

- **Multi-Project Management:** A central dashboard to add, manage, and monitor AI agents across multiple projects.
- **Persistent 24/7 Agents:** Agents keep running in the background. Check your phone and laptop later, and see the results.
- **Mobile-Friendly UI:** Designed for highly flexible vibe coding on any device.
- **File Browser Tree:** Seamlessly browse your project files and insert paths into your prompts.
- **Global & Per-Project Settings:** Configure Claude arguments globally or override them for specific projects directly from the UI.
- **Zero Dependencies:** A single compiled Go binary. No Node.js, npm, or Python required at runtime (only Claude Code itself is a dependency).

## Compile and Usage

### Prerequisites

- **Go** ≥ 1.21 — [go.dev/dl](https://go.dev/dl/) or `brew install go`
- **Claude Code** installed and available as `claude` in your PATH (`npm install -g @anthropic-ai/claude-code`)
- **git**

### Build

Clone the repository and compile the self-contained binary:

```bash
git clone https://github.com/giapnguyen74/claude-web.git
cd claude-web
go build -ldflags="-s -w" -o claude-code-web .
```

Move the compiled `claude-code-web` binary into your standard path (e.g., `/usr/local/bin` or `~/.local/bin`).

### Usage

`claude-code-web` spawns the underlying agent using stdin/stdout stream-json mode. We highly recommend launching it inside a persistent terminal multiplexer like `tmux` or `screen` so the server process itself stays alive:

```bash
# Start a new tmux session
tmux new-session -s claude-server

# Run the server
claude-code-web

# You can now safely detach from the session using: Ctrl+B, D
```

By default, the server binds to loopback only (`127.0.0.1`) on port `4000`. Open your browser to `http://localhost:4000` to access the dashboard. 
From the UI, you can add existing folders from your workspace, create new repositories, or clone from Git.

**Command-Line Options:**
```bash
# Custom port
claude-code-web --port 8080

# Bind to all interfaces (for local network access).
# By default the server binds to loopback (127.0.0.1) only; binding a
# non-loopback host requires a password (see "Password Protection" below),
# otherwise the server refuses to start.
claude-code-web --host 0.0.0.0 --port 4000

# Specify a custom workspace directory for projects
claude-code-web --workspace ~/my-agents-workspace
```

## Security & Setup ⚠️

**`claude-code-web` is meant strictly for local development or within a secure Local/VPN network.**

Do **NOT** expose this server to the public internet. The underlying AI code agent has the ability to execute shell commands and modify files on your machine. Providing public access to this UI is extremely dangerous and effectively gives anyone arbitrary remote code execution capabilities.

### 1. Password Protection

To secure your web UI, you can enforce password authentication.
1. Run `claude-code-web --password` in your terminal.
2. Enter and confirm your password when prompted. The hashed password is saved securely.
3. Restart your `claude-code-web` server. 
Any time you access the web interface or restart the server, you will be required to log in.

### 2. Remote Access (Origins)

If you are accessing the server remotely within a secure network (e.g., a Tailscale VPN), you must explicitly allow your browser's origin. By default, `claude-code-web` strictly blocks cross-origin WebSocket connections to prevent hijacking.
- Launch with the `--origins` flag: `claude-code-web --origins http://my-tailscale-ip:4000`
- Or add it to `~/.claude-code-web/settings.json` under the `"origins"` array.

## License

MIT
