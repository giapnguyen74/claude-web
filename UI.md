# UI Design & Development Guidelines

This document outlines the core UI patterns, CSS variables, and architectural conventions used in the `claude-code-web` project. To maintain a consistent, high-quality, and premium user experience, all future UI additions or modifications must strictly adhere to these standards.

## 1. Aesthetic Identity

The project utilizes a modern, minimalist dark-mode aesthetic loosely inspired by the **Tokyo Night** theme. It relies on subtle contrast, exact spacing, and clean typography rather than heavy shadows or gradients. 

*   **No Tailwind CSS:** We use Vanilla CSS with scoped global variables to keep the project completely dependency-free and ultra-lightweight.
*   **Single-Page Layout:** The app is designed as a rigid, full-screen Flexbox layout (`100dvh`). The `body` itself never scrolls; instead, specific internal containers (like `#conversation` or `#sidebar-list`) scroll independently.
*   **Icon Choice:** We prefer simple, modern, lightweight Unicode symbols, or minimalist, barebones inline SVGs (with `stroke="currentColor"`) for interactive tools. We do not use heavy icon libraries or font-awesome.

## 2. Global CSS Variables

Always use the predefined CSS variables for colors, fonts, and borders. **Never hardcode hex codes or RGB values in new styles.**

```css
:root {
  /* Core Backgrounds */
  --bg:           #1a1b26; /* App background, deep dark */
  --surface:      #16161e; /* Secondary background (Header, Sidebar) */
  --surface2:     #1e2030; /* Tertiary background (Hover states, nested areas) */
  
  /* Borders & Dividers */
  --border:       #2a2b3d; /* Universal border color */
  
  /* Text */
  --text:         #c0caf5; /* Primary text */
  --text-muted:   #565f89; /* Secondary/metadata text */
  
  /* Accents & Status Colors */
  --accent:       #7aa2f7; /* Primary brand/action color (Blue) */
  --green:        #9ece6a; /* Success/Running status */
  --red:          #f7768e; /* Error/Stopped status */
  --yellow:       #e0af68; /* Warning/Starting status */
  --orange:       #ff9e64; /* Warning highlights */
  
  /* Typography */
  --font-sans:    'Inter', system-ui, -apple-system, sans-serif;
  --font-mono:    'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace;
}
```

## 3. Core Layout Structure

The main UI is composed of standard flexbox panels:

*   `#header`: The top navigation bar. Always fixed height (`min-height: 48px`), flex row, border-bottom.
*   `#main-wrapper`: The central container filling the remaining viewport (`flex: 1; display: flex; flex-direction: row;`).
    *   `#sidebar`: The left-hand panel (file tree). Fixed width, flex column, border-right.
    *   `#chat-area`: The primary content area (flex column).
        *   `#conversation`: The scrollable message history (`flex: 1; overflow-y: auto;`).
        *   `#file-viewer-panel`: Replaces the conversation area when viewing files.
        *   `#input-area`: The bottom sticky input box.

### Mobile Responsiveness
On mobile (`@media (max-width: 768px)`):
*   The sidebar expands to `width: 100%`.
*   When the sidebar is open, the `#chat-area` is hidden (`display: none`).
*   When opening a file or clicking an item on mobile, the sidebar is automatically closed via JavaScript (`toggleSidebar()`) to prevent the file viewer from being obscured.

## 4. Components & Interactive Elements

### Buttons
*   **Header Action Buttons:** Use the `.header-action-btn` class. These are transparent buttons with a subtle `--border`, rounded corners (`border-radius: 5px`), and muted text. On hover, the border and text color should transition to an accent or semantic color (e.g., `--accent`, `--red`).
*   **Icon Buttons:** Use lightweight SVG icons with `stroke="currentColor"` so they adapt to the text color.

### Status Badges
Status indicators use `#status-badge` with modifier classes:
*   `.starting`: Yellow background opacity & text.
*   `.running`: Green background opacity & text.
*   `.stopped`: Red background opacity & text.

### Typography
*   Base font size is `14px`.
*   Code, file paths, and technical metadata must use `var(--font-mono)`.
*   Use `var(--text-muted)` for timestamps, secondary labels, and empty states.

## 5. Modals

Modals (e.g., Auth Modal, Origin/CORS Modal) should be completely full-screen (`position: fixed; inset: 0; z-index: 1000;`) and use a semi-transparent backdrop (`background: rgba(0,0,0,0.8); backdrop-filter: blur(4px);`).

The modal card itself should have:
*   `background: var(--surface);`
*   `border: 1px solid var(--border);`
*   `border-radius: 8px;`
*   A subtle `box-shadow`.

## 6. JavaScript & DOM Conventions

*   **No Frameworks:** We use Vanilla JavaScript.
*   **DOM Manipulations:** Keep ID names consistent and unique. Retrieve elements efficiently using `document.getElementById()`.
*   **Security (XSS):** Never use `.innerHTML` for user-generated or API-provided data unless explicitly parsing Markdown. Use `textContent` or `document.createElement()` for dynamic text insertion (e.g., file contents).
*   **Performance:** Cache API requests locally where possible (e.g., `fileTreeCache`).
*   **Event Handling:** When creating interactive elements inside nested structures (like file trees), ensure `event.stopPropagation()` is used so parent containers don't trigger their own click handlers.
