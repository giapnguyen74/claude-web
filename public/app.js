// ── Config ────────────────────────────────────────────────────────────────
// Extract project ID from URL: /project/{id}
const PROJECT_ID = location.pathname.split('/').filter(Boolean).pop() || '';
const API_BASE = `/api/projects/${PROJECT_ID}`;
const WS_PROTOCOL = location.protocol === 'https:' ? 'wss:' : 'ws:';
const WS_URL = `${WS_PROTOCOL}//${location.host}${API_BASE}/events`;

// ── Marked setup ─────────────────────────────────────────────────────────
marked.setOptions({ gfm: true, breaks: true });

// ── Auth ──────────────────────────────────────────────────────────────────
function showAuthModal() {
  document.getElementById('auth-modal').style.display = 'flex';
  document.getElementById('auth-password').focus();
}

async function submitAuth() {
  const pwd = document.getElementById('auth-password').value;
  const errEl = document.getElementById('auth-error');
  try {
    const res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pwd })
    });
    if (!res.ok) {
      errEl.textContent = 'Invalid password';
      return;
    }
    location.reload();
  } catch (err) {
    errEl.textContent = err.message;
  }
}

function renderMarkdown(text) {
  const html = marked.parse(text || '');
  // Sanitize: model output, tool results, and file contents are untrusted and
  // rendered as HTML, so they must be cleaned to prevent XSS. If DOMPurify
  // failed to load, fail safe by escaping rather than injecting raw HTML.
  if (window.DOMPurify) return DOMPurify.sanitize(html);
  console.warn('[security] DOMPurify unavailable — rendering escaped text');
  return esc(text || '');
}

// Add a Copy button to every <pre> code block inside el.
function addCopyButtons(el) {
  el.querySelectorAll('pre').forEach(pre => {
    if (pre.querySelector('.copy-btn')) return;
    const code = pre.querySelector('code');
    if (!code) return;
    const btn = document.createElement('button');
    btn.className = 'copy-btn';
    btn.type = 'button';
    btn.textContent = 'Copy';
    btn.addEventListener('click', async (e) => {
      e.stopPropagation();
      try {
        await navigator.clipboard.writeText(code.innerText);
        btn.textContent = 'Copied';
        btn.classList.add('copied');
      } catch {
        btn.textContent = 'Failed';
      }
      setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('copied'); }, 1500);
    });
    pre.appendChild(btn);
  });
}

function applyHighlight(el) {
  el.querySelectorAll('pre code').forEach(block => hljs.highlightElement(block));
  addCopyButtons(el);
}

// ── Toast ───────────────────────────────────────────────────────────────
let toastTimer = null;
function showToast(msg, type = 'success') {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = `toast show ${type}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 3000);
}

// ── Helpers ───────────────────────────────────────────────────────────────
function esc(str) {
  return String(str)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function scrollToBottom() {
  const c = document.getElementById('conversation');
  c.scrollTop = c.scrollHeight;
}

// When renderTarget is set (history prepend), rows go there instead of the
// live conversation — no working-row shuffle, no auto-scroll.
let renderTarget = null;

function appendRow(row) {
  if (renderTarget) {
    renderTarget.appendChild(row);
    return;
  }
  const c = document.getElementById('conversation');
  c.appendChild(row);
  if (workingRow && workingRow !== row) {
    c.appendChild(workingRow);
  }
  scrollToBottom();
}

// ── DOM refs ──────────────────────────────────────────────────────────────
const conversationEl  = document.getElementById('conversation');
const inputEl         = document.getElementById('message-input');
const sendBtn         = document.getElementById('send-btn');
const stopBtn         = document.getElementById('stop-btn');
const statusBadge     = document.getElementById('status-badge');
const projectNameEl   = document.getElementById('project-name');
const sendingLabel    = document.getElementById('sending-label');
let workingRow = null;

// ── Sidebar Logic ─────────────────────────────────────────────────────────
let fileTreeCache = {}; // path -> files
let expandedDirs = new Set(['']); // empty string is root

function toggleSidebar() {
  const sidebar = document.getElementById('sidebar');
  const wrapper = document.getElementById('main-wrapper');
  if (sidebar.style.display === 'flex') {
    sidebar.style.display = 'none';
    wrapper.classList.remove('sidebar-open');
  } else {
    sidebar.style.display = 'flex';
    wrapper.classList.add('sidebar-open');
    if (!fileTreeCache['']) {
      fetchAndRenderTree();
    }
  }
}

async function fetchAndRenderTree() {
  if (!fileTreeCache['']) {
    await fetchFilesForCache('');
  }
  const listEl = document.getElementById('sidebar-list');
  if (listEl) listEl.innerHTML = renderTreeLevel('', 0);
}

async function refreshTree() {
  const dirs = Array.from(expandedDirs);
  if (!dirs.includes('')) dirs.push('');
  await Promise.all(dirs.map(d => fetchFilesForCache(d)));
  const listEl = document.getElementById('sidebar-list');
  if (listEl) listEl.innerHTML = renderTreeLevel('', 0);
}

async function fetchFilesForCache(dir) {
  try {
    const res = await fetch(`${API_BASE}/files?dir=${encodeURIComponent(dir)}`);
    if (!res.ok) throw new Error('Failed to fetch files');
    const data = await res.json();
    fileTreeCache[dir] = data.files.sort((a, b) => {
      if (a.isDir && !b.isDir) return -1;
      if (!a.isDir && b.isDir) return 1;
      return a.name.localeCompare(b.name);
    });
  } catch (err) {
    console.error(err);
    delete fileTreeCache[dir];
    expandedDirs.delete(dir);
  }
}

async function toggleDir(dirPath, e) {
  if (e) e.stopPropagation();
  
  if (expandedDirs.has(dirPath)) {
    expandedDirs.delete(dirPath);
  } else {
    expandedDirs.add(dirPath);
    if (!fileTreeCache[dirPath]) {
      await fetchFilesForCache(dirPath);
    }
  }
  const listEl = document.getElementById('sidebar-list');
  listEl.innerHTML = renderTreeLevel('', 0);
}

function handleItemClick(path, isDir, event) {
  if (event) event.stopPropagation();
  if (isDir) {
    toggleDir(path);
  } else {
    openFileViewer(path);
  }
}

function renderTreeLevel(dirPath, depth) {
  const files = fileTreeCache[dirPath];
  if (!files) return '';
  
  let html = '';
  for (const f of files) {
    if (f.name.startsWith('.')) continue;
    
    const targetPath = dirPath === '' ? f.name : `${dirPath}/${f.name}`;
    const padding = depth * 16 + 16;
    
    if (f.isDir) {
      const isExpanded = expandedDirs.has(targetPath);
      const icon = isExpanded ? '📂' : '📁';
      html += `
        <div class="file-item dir" style="padding-left: ${padding}px">
          <div class="file-item-icon" onclick="handleItemClick('${esc(targetPath)}', true, event)">${icon}</div>
          <div style="flex:1" onclick="handleItemClick('${esc(targetPath)}', true, event)">${esc(f.name)}</div>
          <button class="file-view-btn" onclick="openDropdown(event, '${esc(targetPath)}', true)" title="More Actions">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="display:block;"><circle cx="12" cy="12" r="1"></circle><circle cx="12" cy="5" r="1"></circle><circle cx="12" cy="19" r="1"></circle></svg>
          </button>
        </div>
      `;
      if (isExpanded) {
        html += renderTreeLevel(targetPath, depth + 1);
      }
    } else {
      html += `
        <div class="file-item" style="padding-left: ${padding}px">
          <div class="file-item-icon" onclick="handleItemClick('${esc(targetPath)}', false, event)">📄</div>
          <div style="flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;" onclick="handleItemClick('${esc(targetPath)}', false, event)">${esc(f.name)}</div>
          <button class="file-view-btn" onclick="openDropdown(event, '${esc(targetPath)}', false)" title="More Actions">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="display:block;"><circle cx="12" cy="12" r="1"></circle><circle cx="12" cy="5" r="1"></circle><circle cx="12" cy="19" r="1"></circle></svg>
          </button>
        </div>
      `;
    }
  }
  return html;
}

function insertFilePath(path, event) {
  if (event) event.stopPropagation();
  const input = document.getElementById('message-input');
  const val = input.value;
  if (val && !val.endsWith(' ')) {
    input.value = val + ' ' + path + ' ';
  } else {
    input.value = val + path + ' ';
  }
  input.focus();
  updateInputUI(); // update button state
  
  // Mobile only: Close sidebar when inserting a file
  if (window.innerWidth <= 768) {
    toggleSidebar();
  }
}

function downloadFile(path, event) {
  if (event) event.stopPropagation();
  const a = document.createElement('a');
  a.href = `${API_BASE}/files/download?path=${encodeURIComponent(path)}`;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

function downloadCurrentFile() {
  if (currentFilePath) {
    downloadFile(currentFilePath);
  }
}

function uploadToFolder(path, event) {
  if (event) event.stopPropagation();
  const input = document.createElement('input');
  input.type = 'file';
  input.onchange = async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    const formData = new FormData();
    formData.append('file', file);
    
    try {
      const res = await fetch(`${API_BASE}/files/upload?path=${encodeURIComponent(path)}`, {
        method: 'POST',
        body: formData
      });
      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        alert("Upload failed: " + (errData.error || res.statusText));
      } else {
        refreshTree();
      }
    } catch (err) {
      alert("Upload failed: " + err.message);
    }
  };
  input.click();
}

// ── Dropdown Logic ────────────────────────────────────────────────────────
let dropdownOpen = false;

function closeDropdown() {
  const dd = document.getElementById('action-dropdown');
  if (dd) {
    dd.style.display = 'none';
    dropdownOpen = false;
  }
}

window.addEventListener('click', (e) => {
  if (dropdownOpen && !e.target.closest('.dropdown-menu') && !e.target.closest('.file-view-btn')) {
    closeDropdown();
  }
}, { capture: true });

// Close dropdown on any scroll (since it's position: fixed)
window.addEventListener('scroll', () => {
  if (dropdownOpen) closeDropdown();
}, { capture: true, passive: true });

// Close dropdown on touch move or outside touch (for mobile)
window.addEventListener('touchstart', (e) => {
  if (dropdownOpen && !e.target.closest('.dropdown-menu') && !e.target.closest('.file-view-btn')) {
    closeDropdown();
  }
}, { passive: true });

function openDropdown(event, path, isDir) {
  event.stopPropagation();
  event.preventDefault();
  closeDropdown();

  const dd = document.getElementById('action-dropdown');
  dd.innerHTML = '';
  
  if (isDir) {
    dd.innerHTML += `<button class="dropdown-item" onclick="insertFilePath('${esc(path)}/', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg> Insert Path</button>`;
    dd.innerHTML += `<button class="dropdown-item" onclick="uploadToFolder('${esc(path)}', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="17 8 12 3 7 8"></polyline><line x1="12" y1="3" x2="12" y2="15"></line></svg> Upload File</button>`;
    dd.innerHTML += `<button class="dropdown-item" onclick="downloadFile('${esc(path)}', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg> Download ZIP</button>`;
  } else {
    dd.innerHTML += `<button class="dropdown-item" onclick="insertFilePath('${esc(path)}', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg> Insert Path</button>`;
    dd.innerHTML += `<button class="dropdown-item" onclick="downloadFile('${esc(path)}', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg> Download File</button>`;
  }
  
  dd.innerHTML += `<div style="height:1px; background:var(--border); margin:4px 0;"></div>`;
  dd.innerHTML += `<button class="dropdown-item danger" onclick="deleteFile('${esc(path)}', event)"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg> Delete</button>`;

  dd.style.display = 'flex';
  
  // Position it
  const rect = event.currentTarget.getBoundingClientRect();
  let top = rect.bottom + 4;
  let left = rect.right - 160; // align right roughly
  
  // Need to make sure it doesn't go off screen bottom
  // We don't have accurate height until displayed, but we can guess ~130px max
  if (top + 130 > window.innerHeight) {
    top = rect.top - 130; 
  }
  
  dd.style.top = top + 'px';
  dd.style.left = left + 'px';
  
  dropdownOpen = true;
}

async function deleteFile(path, event) {
  if (event) event.stopPropagation();
  closeDropdown();
  if (!confirm(`Are you sure you want to permanently delete '${path}'?\nThis cannot be undone.`)) {
    return;
  }
  
  try {
    const res = await fetch(`${API_BASE}/files/delete?path=${encodeURIComponent(path)}`, { method: 'DELETE' });
    if (!res.ok) {
      const errData = await res.json().catch(() => ({}));
      alert("Delete failed: " + (errData.error || res.statusText));
    } else {
      refreshTree();
    }
  } catch (err) {
    alert("Delete failed: " + err.message);
  }
}

// ── Working spinner ───────────────────────────────────────────────────────
const SPINNER_FRAMES = ['⠋','⠙','⠹','⠸','⠼','⠴','⠦','⠧','⠇','⠏'];
let spinnerIdx   = 0;
let workingStart = null;
let workingTimer = null;

function formatElapsed(ms) {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `(${s}s)`;
  return `(${Math.floor(s / 60)}m ${s % 60}s)`;
}

// Set while rendering older history into a detached fragment, so the live
// working-spinner timer is never started or cleared as a side effect.
let historyRendering = false;

function startWorking() {
  if (historyRendering) return;
  if (workingTimer) return; // already running
  workingStart = Date.now();
  spinnerIdx = 0;

  workingRow = document.createElement('div');
  workingRow.className = 'msg-row working-row';
  workingRow.innerHTML = `
    <div class="msg-label assistant">Claude</div>
    <div class="msg-body working-body">
      <span class="working-spinner-inline">${SPINNER_FRAMES[0]}</span>
      <span>Working...</span>
      <span class="working-elapsed-inline">(0s)</span>
    </div>
  `;
  appendRow(workingRow);

  const spinnerEl = workingRow.querySelector('.working-spinner-inline');
  const elapsedEl = workingRow.querySelector('.working-elapsed-inline');

  workingTimer = setInterval(() => {
    if (spinnerEl) {
      spinnerEl.textContent = SPINNER_FRAMES[spinnerIdx++ % SPINNER_FRAMES.length];
    }
    if (elapsedEl) {
      elapsedEl.textContent = formatElapsed(Date.now() - workingStart);
    }
  }, 100);
}

function stopWorking() {
  if (historyRendering) return;
  if (workingTimer) {
    clearInterval(workingTimer);
    workingTimer = null;
  }
  workingStart = null;
  if (workingRow) {
    workingRow.remove();
    workingRow = null;
  }
}

// ── State ─────────────────────────────────────────────────────────────────
let sessionStatus = 'starting';
let currentProjectName = '';
let currentClaudeArgs = [];

// Current streaming assistant message row
let streamingRow      = null;  // the .msg-row div
let streamingBody     = null;  // the .msg-body div inside it
let streamingText     = '';    // accumulated raw text

// ── Status ────────────────────────────────────────────────────────────────
function setStatus(status, sessionId) {
  const prev = sessionStatus;
  sessionStatus = status;
  const labels = { starting: '● starting', running: '● running', stopped: '○ stopped', idle: '○ idle' };
  statusBadge.textContent = labels[status] || status;
  statusBadge.className = status;
  sendBtn.disabled = (status !== 'running');

  // Mark the real end of a session (process exited), not per-turn completion.
  if (status === 'stopped' && (prev === 'running' || prev === 'starting') && !isReplaying) {
    const note = document.createElement('div');
    note.className = 'notice';
    note.textContent = 'session ended';
    appendRow(note);
  }
  // Show/hide start/stop buttons based on status
  const startBtn = document.getElementById('start-btn');
  const stopBtn = document.getElementById('stop-btn');
  if (status === 'idle' || status === 'stopped') {
    startBtn.style.display = 'inline-block';
    stopBtn.style.display = 'none';
  } else {
    startBtn.style.display = 'none';
    stopBtn.style.display = 'inline-block';
  }
}

// ── Toggle collapsible cards ──────────────────────────────────────────────
function toggleCard(id) {
  const el = document.getElementById(id);
  if (!el) return;
  const body   = el.querySelector('.tool-card-body, .result-card-body');
  const toggle = el.querySelector('.tool-card-toggle');
  if (!body) return;
  const collapsed = body.classList.toggle('hidden');
  if (toggle) toggle.textContent = collapsed ? '▾ show' : '▴ hide';
}

// ── Render a tool-use block ───────────────────────────────────────────────
function renderToolUseBlock(block) {
  const id = `tc-${block.id || Math.random().toString(36).slice(2)}`;
  const inputJson = JSON.stringify(block.input, null, 2);
  const div = document.createElement('div');
  div.className = 'tool-card';
  div.id = id;
  div.innerHTML = `
    <div class="tool-card-header" onclick="toggleCard('${id}')">
      <span class="tool-card-icon">⚙</span>
      <span class="tool-card-name">${esc(block.name || 'tool')}</span>
      <span class="tool-card-toggle">▾ show</span>
    </div>
    <div class="tool-card-body hidden">${esc(inputJson)}</div>
  `;
  return div;
}

// ── Render a tool-result block ────────────────────────────────────────────
function renderToolResultBlock(block) {
  const id = `rc-${block.tool_use_id || Math.random().toString(36).slice(2)}`;
  const content = typeof block.content === 'string'
    ? block.content
    : JSON.stringify(block.content, null, 2);
  const div = document.createElement('div');
  div.className = 'result-card';
  div.id = id;
  div.innerHTML = `
    <div class="result-card-header" onclick="toggleCard('${id}')">▾ tool result</div>
    <div class="result-card-body hidden">${esc(content)}</div>
  `;
  return div;
}

// ── Render user message ───────────────────────────────────────────────────
function renderUserEvent(ev) {
  if (!ev.message?.content) return;

  const textBlocks   = ev.message.content.filter(b => b.type === 'text' && b.text);
  const resultBlocks = ev.message.content.filter(b => b.type === 'tool_result');

  // Plain user text → "You" row
  if (textBlocks.length > 0) {
    const text = textBlocks.map(b => b.text).join('\n');
    const row = document.createElement('div');
    row.className = 'msg-row';
    row.innerHTML = `
      <div class="msg-label user">You</div>
      <div class="msg-body"><div class="user-text">${esc(text)}</div></div>
    `;
    appendRow(row);
  }

  // Tool results → compact result cards (no "You" label)
  if (resultBlocks.length > 0) {
    const row = document.createElement('div');
    row.className = 'msg-row';
    const body = document.createElement('div');
    body.className = 'msg-body';
    for (const block of resultBlocks) {
      body.appendChild(renderToolResultBlock(block));
    }
    row.appendChild(body);
    appendRow(row);
  }
}

// ── Streaming: start / append / finalize ─────────────────────────────────

function startStreamingMessage() {
  // Clean up any orphaned streaming row (e.g. during replay)
  if (streamingRow) {
    streamingRow.dataset.streaming = '';
    streamingRow = null;
    streamingBody = null;
    streamingText = '';
  }

  const row = document.createElement('div');
  row.className = 'msg-row';
  row.dataset.streaming = 'true';
  row.innerHTML = `
    <div class="msg-label assistant">Claude</div>
    <div class="msg-body"><span class="md streaming"></span></div>
  `;
  appendRow(row);
  streamingRow  = row;
  streamingBody = row.querySelector('.md');
  streamingText = '';
}

function appendStreamingText(text) {
  if (!streamingBody) startStreamingMessage();
  streamingText += text;
  // Show raw text during streaming (fast, no markdown processing)
  streamingBody.textContent = streamingText;
  streamingBody.classList.add('streaming');
  scrollToBottom();
}

// ── Render completed assistant message (replaces streaming row) ───────────
function renderAssistantEvent(ev) {
  // Remove the streaming placeholder
  if (streamingRow) {
    streamingRow.remove();
    streamingRow  = null;
    streamingBody = null;
    streamingText = '';
  }

  if (!ev.message?.content) return;

  const row = document.createElement('div');
  row.className = 'msg-row';

  const label = document.createElement('div');
  label.className = 'msg-label assistant';
  label.textContent = 'Claude';

  const body = document.createElement('div');
  body.className = 'msg-body';

  for (const block of ev.message.content) {
    if (block.type === 'text' && block.text) {
      const md = document.createElement('div');
      md.className = 'md';
      md.innerHTML = renderMarkdown(block.text);
      applyHighlight(md);
      body.appendChild(md);
    } else if (block.type === 'tool_use') {
      body.appendChild(renderToolUseBlock(block));
    }
  }

  // Token usage
  if (ev.message.usage) {
    const { input_tokens, output_tokens } = ev.message.usage;
    const usage = document.createElement('div');
    usage.className = 'usage-line';
    usage.textContent = `${input_tokens} in · ${output_tokens} out`;
    body.appendChild(usage);
  }

  row.appendChild(label);
  row.appendChild(body);
  appendRow(row);
}

// ── Approval cards ────────────────────────────────────────────────────────
const pendingApprovals = {};

function renderApprovalCard(ev) {
  const { request_id, request } = ev;
  const inputJson = JSON.stringify(request.input, null, 2);

  const row = document.createElement('div');
  row.className = 'approval-row';
  row.innerHTML = `
    <div class="approval-card" id="ap-card-${esc(request_id)}">
      <div class="approval-title">⚠ Tool request</div>
      <div class="approval-tool-name">${esc(request.tool_name || 'unknown')}</div>
      <div class="approval-input">${esc(inputJson)}</div>
      <div class="approval-actions">
        <button class="btn btn-allow" id="ap-allow-${esc(request_id)}"
          onclick="sendApproval('${esc(request_id)}', true)">Allow</button>
        <button class="btn btn-deny" id="ap-deny-${esc(request_id)}"
          onclick="sendApproval('${esc(request_id)}', false)">Deny</button>
        <span class="approval-outcome" id="ap-outcome-${esc(request_id)}"></span>
      </div>
    </div>
  `;

  appendRow(row);
  pendingApprovals[request_id] = row;
}

function resolveApprovalCard(requestId, allowed) {
  const row = document.getElementById(`ap-card-${requestId}`);
  if (!row) return;

  row.classList.add('resolved');
  const allowBtn  = document.getElementById(`ap-allow-${requestId}`);
  const denyBtn   = document.getElementById(`ap-deny-${requestId}`);
  const outcomeEl = document.getElementById(`ap-outcome-${requestId}`);

  if (allowBtn)  allowBtn.disabled  = true;
  if (denyBtn)   denyBtn.disabled   = true;
  if (outcomeEl) {
    outcomeEl.classList.add('show');
    if (allowed === true) {
      outcomeEl.classList.add('allowed');
      outcomeEl.textContent = '✓ Allowed';
    } else if (allowed === false) {
      outcomeEl.classList.add('denied');
      outcomeEl.textContent = '✗ Denied';
    } else {
      outcomeEl.textContent = 'Resolved';
    }
  }

  delete pendingApprovals[requestId];
}

async function sendApproval(requestId, allowed) {
  const allowBtn = document.getElementById(`ap-allow-${requestId}`);
  const denyBtn  = document.getElementById(`ap-deny-${requestId}`);
  if (allowBtn) allowBtn.disabled = true;
  if (denyBtn)  denyBtn.disabled  = true;

  try {
    await fetch(`${API_BASE}/approve`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ requestId, allowed }),
    });
  } catch (err) {
    console.error('Failed to send approval:', err);
    if (allowBtn) allowBtn.disabled = false;
    if (denyBtn)  denyBtn.disabled  = false;
  }
}

// ── Main event handler ────────────────────────────────────────────────────
let isReplaying = false;

function handleClaudeEvent(ev) {
  switch (ev.type) {

    case 'system':
      if (ev.subtype === 'session_start') {
        const cwd  = ev.data?.cwd || '';
        const name = cwd.split('/').filter(Boolean).pop() || cwd || '—';
        projectNameEl.textContent = cwd || name;
        if (ev.data?.model) {
          showRunningModel(ev.data.model);
        }
        const note = document.createElement('div');
        note.className = 'notice';
        note.textContent = `session started · ${cwd}`;
        appendRow(note);
      } else if (ev.subtype === 'turn_end') {
        // End of one turn — the session stays alive for the next message.
        stopWorking();
      } else if (ev.subtype === 'session_end') {
        // True session end (kept for older history logs that used this marker).
        stopWorking();
        const note = document.createElement('div');
        note.className = 'notice';
        note.textContent = 'session ended';
        appendRow(note);
      }
      break;

    case 'stream_event': {
      const sev = ev.event;
      if (!sev) break;
      if (sev.type === 'message_start') {
        startStreamingMessage();
        if (!isReplaying) startWorking();
      } else if (
        sev.type === 'content_block_delta' &&
        sev.delta?.type === 'text_delta' &&
        sev.delta.text
      ) {
        appendStreamingText(sev.delta.text);
      }
      break;
    }

    case 'assistant':
      stopWorking();
      renderAssistantEvent(ev);
      break;

    case 'user':
      renderUserEvent(ev);
      break;

    case 'control_request':
      stopWorking(); // waiting for human approval — no longer "working"
      renderApprovalCard(ev);
      break;

    case 'control_response':
      if (ev.response?.request_id) {
        const allowed = ev.response.response?.allowed;
        resolveApprovalCard(ev.response.request_id, allowed);
      }
      break;
  }
}

// ── WebSocket ─────────────────────────────────────────────────────────────
let reconnectDelay = 2000;
let isWsConnected = false;
let currentWs = null;
let reconnectTimer = null;

// ── CORS / Origin Check ───────────────────────────────────────────────────
fetch(`/api/check-origin?origin=${encodeURIComponent(location.origin)}`)
  .then(r => {
    if (r.status === 403) {
      document.querySelectorAll('.warning-origin-url').forEach(el => el.textContent = location.origin);
      document.getElementById('origin-modal').style.display = 'flex';
    }
  })
  .catch(console.error);

function scheduleReconnect() {
  if (reconnectTimer) return; // already scheduled
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    reconnectDelay = Math.min(reconnectDelay * 1.5, 30000);
    connect();
  }, reconnectDelay);
}

// Reconnect immediately (used when the tab regains focus or the network
// returns) instead of waiting out the backoff — important for phones that
// suspend the socket while asleep.
function reconnectNow() {
  if (currentWs && currentWs.readyState === WebSocket.OPEN) return;
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  // Drop any stale socket so its onclose doesn't double-schedule a reconnect.
  if (currentWs && currentWs.readyState !== WebSocket.CLOSED) {
    try { currentWs.onclose = null; currentWs.close(); } catch (_) {}
  }
  reconnectDelay = 2000;
  connect();
}

function connect() {
  const ws = new WebSocket(WS_URL);
  currentWs = ws;

  ws.onopen = () => {
    console.log('[ws] connected');
    reconnectDelay = 2000; // reset delay on successful connection
    isWsConnected = true;
  };

  ws.onmessage = (e) => {
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }

    if (msg.type === 'server_status') {
      setStatus(msg.status, msg.sessionId);
      return;
    }

    if (msg.type === 'replay_start') {
      // Clear conversation and reset streaming state before replay
      isReplaying = true;
      stopWorking();
      conversationEl.innerHTML = '';
      streamingRow  = null;
      streamingBody = null;
      streamingText = '';
      // History cursor: where the replayed tail begins, and whether older
      // events exist to load on scroll-up.
      historyFirstIndex = msg.firstIndex || 0;
      historyHasMore = !!msg.hasMore;
      historyLoading = false;
      return;
    }

    if (msg.type === 'replay_end') {
      isReplaying = false;
      scrollToBottom();
      maybeAutoFillHistory();
      return;
    }

    if (msg.type === 'claude_event') {
      handleClaudeEvent(msg.data);
    }
  };

  ws.onclose = () => {
    console.log(`[ws] disconnected — reconnecting in ${reconnectDelay / 1000}s…`);
    isWsConnected = false;
    scheduleReconnect();
  };

  ws.onerror = (err) => {
    console.error('[ws] error', err);
    isWsConnected = false;
  };
}

connect();

// Reconnect promptly when the tab becomes visible or connectivity returns.
document.addEventListener('visibilitychange', () => { if (!document.hidden) reconnectNow(); });
window.addEventListener('online', reconnectNow);
window.addEventListener('focus', reconnectNow);

// ── History (scroll-up to load older conversations) ────────────────────────
let historyFirstIndex = 0;
let historyHasMore = false;
let historyLoading = false;

function setHistoryLoader(on) {
  const c = document.getElementById('conversation');
  let el = document.getElementById('history-loader');
  if (on) {
    if (!el) {
      el = document.createElement('div');
      el.id = 'history-loader';
      el.className = 'notice';
      el.textContent = 'loading earlier messages…';
    }
    c.insertBefore(el, c.firstChild);
  } else if (el) {
    el.remove();
  }
}

async function loadOlderHistory() {
  if (historyLoading || !historyHasMore) return;
  historyLoading = true;
  setHistoryLoader(true);
  try {
    const res = await fetch(`${API_BASE}/events/history?before=${historyFirstIndex}&limit=300`);
    if (!res.ok) return;
    const data = await res.json();
    const events = data.events || [];

    const c = document.getElementById('conversation');
    setHistoryLoader(false);

    // Anchor the viewport so the page doesn't jump when we prepend.
    const prevHeight = c.scrollHeight;
    const prevTop = c.scrollTop;

    // Render the older chunk into a fragment with the existing renderers, while
    // isolating live render state. This block is fully synchronous, so no WS
    // event can interleave and corrupt the saved globals.
    const frag = document.createDocumentFragment();
    const saved = { renderTarget, isReplaying, streamingRow, streamingBody, streamingText };
    renderTarget = frag;
    isReplaying = true;
    historyRendering = true; // guards start/stopWorking against the live spinner
    streamingRow = null; streamingBody = null; streamingText = '';
    for (const ev of events) {
      if (ev && ev.type === 'stream_event') continue; // skip live-typing deltas
      try { handleClaudeEvent(ev); } catch (err) { console.error('history render', err); }
    }
    historyRendering = false;
    renderTarget = saved.renderTarget;
    isReplaying = saved.isReplaying;
    streamingRow = saved.streamingRow;
    streamingBody = saved.streamingBody;
    streamingText = saved.streamingText;

    c.insertBefore(frag, c.firstChild);
    if (typeof data.firstIndex === 'number') historyFirstIndex = data.firstIndex;
    historyHasMore = !!data.hasMore;

    c.scrollTop = c.scrollHeight - prevHeight + prevTop;
    // If it still doesn't scroll, keep filling (runs after this call settles).
    setTimeout(maybeAutoFillHistory, 0);
  } catch (err) {
    console.error('Failed to load history', err);
    setHistoryLoader(false);
  } finally {
    historyLoading = false;
  }
}

// If the current session is short enough that nothing scrolls but older
// history exists, pull a page so it's reachable (and keep filling until the
// view scrolls or history is exhausted).
function maybeAutoFillHistory() {
  const c = document.getElementById('conversation');
  if (historyHasMore && !historyLoading && c.scrollHeight <= c.clientHeight + 4) {
    loadOlderHistory();
  }
}

conversationEl.addEventListener('scroll', () => {
  if (conversationEl.scrollTop < 120 && historyHasMore && !historyLoading && !isReplaying) {
    loadOlderHistory();
  }
});

// ── Voice Input State & Input UI State ─────────────────────────────────────
const micBtn = document.getElementById('mic-btn');
const cancelBtn = document.getElementById('cancel-btn');
let speechRecognition = null;
let isListening = false;
let autoSendOnEnd = false;

function updateInputUI() {
  const hasText = inputEl.value.trim().length > 0;
  
  if (isListening) {
    // Recording: Hide Send, show Mic and Cancel
    sendBtn.style.display = 'none';
    cancelBtn.style.display = 'flex';
    micBtn.style.display = 'flex';
  } else if (hasText) {
    // Typing: Hide Mic, show Cancel and Send
    micBtn.style.display = 'none';
    cancelBtn.style.display = 'flex';
    sendBtn.style.display = 'block';
  } else {
    // Default (Empty): Show Mic, hide Cancel and Send
    micBtn.style.display = 'flex';
    cancelBtn.style.display = 'none';
    sendBtn.style.display = 'none';
  }
}

// Initial UI state
updateInputUI();

// ── Send message ──────────────────────────────────────────────────────────
async function sendMessage() {
  const text = inputEl.value.trim();
  if (!text) return;
  if (sessionStatus !== 'running') {
    // Keep the typed text and tell the user why nothing was sent.
    showToast(
      sessionStatus === 'stopped'
        ? 'Session stopped — press ▶ start to resume, then send.'
        : 'Session is still starting — try again in a moment.',
      'error'
    );
    return;
  }

  inputEl.value = '';
  inputEl.style.height = 'auto';
  updateInputUI();
  sendBtn.disabled = true;
  sendingLabel.classList.add('show');

  // Start working spinner immediately so the user knows the bot is working!
  startWorking();

  try {
    const res = await fetch(`${API_BASE}/message`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text }),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      console.error('Send failed:', err);
      stopWorking();
    }
  } catch (err) {
    console.error('Send error:', err);
    stopWorking();
  } finally {
    sendingLabel.classList.remove('show');
    sendBtn.disabled = (sessionStatus !== 'running');
    inputEl.focus();
  }
}

sendBtn.addEventListener('click', sendMessage);

inputEl.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendMessage();
  }
});

// Auto-resize textarea
inputEl.addEventListener('input', () => {
  inputEl.style.height = 'auto';
  inputEl.style.height = Math.min(inputEl.scrollHeight, 200) + 'px';
  updateInputUI();
});

// Start button
document.getElementById('start-btn').addEventListener('click', async () => {
  try {
    const res = await fetch(`${API_BASE}/start`, { method: 'POST' });
    if (res.ok) {
      // Reconnect WS to get the new session
      location.reload();
    }
  } catch (err) {
    console.error('Start error:', err);
  }
});

// Stop button
stopBtn.addEventListener('click', async () => {
  if (!confirm('Stop the Claude Code session?')) return;
  try {
    await fetch(`${API_BASE}/stop`, { method: 'POST' });
  } catch (err) {
    console.error('Stop error:', err);
  }
});

// ── Voice Input & Cancel ──────────────────────────────────────────────────

cancelBtn.addEventListener('click', () => {
  inputEl.value = '';
  inputEl.style.height = 'auto';
  if (isListening && speechRecognition) {
    autoSendOnEnd = false;
    speechRecognition.stop();
  }
  updateInputUI();
});

if ('webkitSpeechRecognition' in window || 'SpeechRecognition' in window) {
  const SpeechRec = window.SpeechRecognition || window.webkitSpeechRecognition;
  speechRecognition = new SpeechRec();
  speechRecognition.continuous = false;
  speechRecognition.interimResults = false;

  const stopListening = () => {
    isListening = false;
    micBtn.innerHTML = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" y1="19" x2="12" y2="22"/></svg>`;
    micBtn.classList.remove('listening');
    updateInputUI();
  };

  speechRecognition.onstart = () => {
    isListening = true;
    autoSendOnEnd = false;
    micBtn.innerHTML = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="7" y="7" width="10" height="10" rx="2" ry="2"/></svg>`;
    micBtn.classList.add('listening');
    updateInputUI();
  };

  speechRecognition.onresult = (event) => {
    const transcript = event.results[0][0].transcript;
    const currentVal = inputEl.value;
    inputEl.value = currentVal ? currentVal + ' ' + transcript : transcript;
    
    // Auto-resize trigger
    inputEl.style.height = 'auto';
    inputEl.style.height = Math.min(inputEl.scrollHeight, 200) + 'px';
  };

  speechRecognition.onerror = (event) => {
    console.error('Speech recognition error:', event.error);
    stopListening();
  };

  speechRecognition.onend = () => {
    stopListening();
    if (autoSendOnEnd && inputEl.value.trim() !== '') {
      sendMessage();
    }
  };

  micBtn.addEventListener('click', () => {
    if (isListening) {
      autoSendOnEnd = true;
      speechRecognition.stop();
    } else {
      speechRecognition.lang = localStorage.getItem('claude_dictate_lang') || 'en-US';
      speechRecognition.start();
    }
  });
} else {
  micBtn.style.opacity = '0.4';
  micBtn.style.cursor = 'not-allowed';
  micBtn.addEventListener('click', () => {
    alert('Voice input is disabled by your browser. Apple and Google require the web app to be accessed via HTTPS (secure context) or localhost to use the microphone.');
  });
}

// Fetch initial status (shows project dir even before first WS message)
fetch(`${API_BASE}/status`)
  .then(r => {
    if (r.status === 401) {
      showAuthModal();
      return null;
    }
    return r.json();
  })
  .then(data => {
    if (!data) return;
    if (data.name) {
      projectNameEl.textContent = data.name;
      currentProjectName = data.name;
    } else if (data.projectDir) {
      projectNameEl.textContent = data.projectDir;
      currentProjectName = data.projectDir.split('/').pop();
    }
    currentClaudeArgs = data.claudeArgs || data.qwenArgs || [];
    updateSelectorFromArgs(currentClaudeArgs);
    setStatus(data.status, data.sessionId);
  })
  .catch(console.error);

// ── Model helpers & actions ────────────────────────────────────────────────
function getModelFromArgs(args) {
  if (!args) return '';
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--model' && i + 1 < args.length) {
      return args[i + 1];
    }
  }
  return '';
}

function updateSelectorFromArgs(args) {
  const model = getModelFromArgs(args);
  const selector = document.getElementById('model-selector');
  if (!selector) return;

  if (!model) {
    selector.value = 'sonnet';
    return;
  }

  if (model === 'sonnet' || model.includes('sonnet')) {
    selector.value = 'sonnet';
  } else if (model === 'opus' || model.includes('opus')) {
    selector.value = 'opus';
  } else if (model === 'haiku' || model.includes('haiku')) {
    selector.value = 'haiku';
  } else {
    selector.value = 'custom';
    const customOpt = selector.querySelector('option[value="custom"]');
    if (customOpt) {
      customOpt.textContent = `Custom (${model})`;
    }
  }
}

function showRunningModel(modelName) {
  const display = document.getElementById('model-display');
  const selector = document.getElementById('model-selector');
  if (display && selector) {
    display.textContent = modelName;
    display.style.display = 'inline';
    selector.style.display = 'none';
  }
}

function resetModelDisplay() {
  const display = document.getElementById('model-display');
  const selector = document.getElementById('model-selector');
  if (display && selector) {
    display.style.display = 'none';
    selector.style.display = 'inline';
    selector.disabled = false;
  }
}

async function changeModel() {
  const selector = document.getElementById('model-selector');
  let selectedModel = selector.value;
  if (selectedModel === 'custom') {
    const custom = prompt("Enter custom model name (e.g. 'claude-3-5-sonnet-20241022'):");
    if (!custom) {
      updateSelectorFromArgs(currentClaudeArgs);
      return;
    }
    selectedModel = custom;
  }

  // Build new args list
  let newArgs = [...currentClaudeArgs];
  let found = false;
  for (let i = 0; i < newArgs.length; i++) {
    if (newArgs[i] === '--model' && i + 1 < newArgs.length) {
      newArgs[i + 1] = selectedModel;
      found = true;
      break;
    }
  }
  if (!found) {
    newArgs.push('--model', selectedModel);
  }

  try {
    const res = await fetch(`/api/projects/${PROJECT_ID}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: currentProjectName, claudeArgs: newArgs, qwenArgs: newArgs }),
    });
    if (res.ok) {
      currentClaudeArgs = newArgs;
      showToast('Model updated successfully');
      updateSelectorFromArgs(newArgs);
    } else {
      const data = await res.json().catch(() => ({}));
      alert('Failed to save model: ' + (data.error || res.statusText));
      updateSelectorFromArgs(currentClaudeArgs);
    }
  } catch (err) {
    alert('Failed to save model: ' + err.message);
    updateSelectorFromArgs(currentClaudeArgs);
  }
}

function showToast(msg, type = 'success') {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = `toast show ${type}`;
  setTimeout(() => el.className = 'toast', 3000);
}

// ── File Viewer ───────────────────────────────────────────────────────────
let currentFileContent = '';
let isMarkdownRendered = false;

function toggleMarkdownView() {
  isMarkdownRendered = !isMarkdownRendered;
  const btn = document.getElementById('md-toggle-btn');
  const content = document.getElementById('fv-content');
  
  if (isMarkdownRendered) {
    btn.textContent = 'RAW';
    content.innerHTML = '<div class="markdown-body" style="padding: 20px;">' + renderMarkdown(currentFileContent) + '</div>';
    if (window.hljs) {
      content.querySelectorAll('pre code').forEach(block => {
        hljs.highlightElement(block);
      });
    }
    addCopyButtons(content);
  } else {
    btn.textContent = '👁 Render';
    const pre = document.createElement('pre');
    pre.style.cssText = "margin:0; padding:16px; font-size:13px; font-family:var(--font-mono); min-height:100%; overflow-x:auto;";
    const code = document.createElement('code');
    code.className = "language-markdown hljs";
    code.style.cssText = "background:transparent; padding:0;";
    code.textContent = currentFileContent;
    pre.appendChild(code);

    content.innerHTML = '';
    content.appendChild(pre);

    if (window.hljs) {
      hljs.highlightElement(code);
    }
    addCopyButtons(content);
  }
}

async function openFileViewer(path, event) {
  if (event) event.stopPropagation();
  
  // Mobile only: Close sidebar when opening a file so it doesn't cover the viewer
  if (window.innerWidth <= 768) {
    const sidebar = document.getElementById('sidebar');
    if (sidebar.style.display === 'flex') {
      toggleSidebar();
    }
  }

  const panel = document.getElementById('file-viewer-panel');
  const conv = document.getElementById('conversation');
  const title = document.getElementById('fv-title');
  const content = document.getElementById('fv-content');
  const mdToggleBtn = document.getElementById('md-toggle-btn');
  
  title.textContent = path;
  content.innerHTML = '<div style="color:var(--text-muted); text-align:center; padding: 40px;">Loading...</div>';
  mdToggleBtn.style.display = 'none';
  
  // Swap panels
  conv.style.display = 'none';
  panel.style.display = 'flex';

  try {
    const res = await fetch(`${API_BASE}/files/read?path=${encodeURIComponent(path)}`);
    if (!res.ok) {
      const errData = await res.json().catch(() => ({}));
      content.innerHTML = `<div style="color:var(--red); padding: 20px; font-family:var(--font-mono); font-size:13px;">Error: ${esc(errData.error || 'Failed to load file')}</div>`;
      return;
    }
    const data = await res.json();
    
    if (data.type === 'text') {
      const isMd = path.toLowerCase().endsWith('.md') || path.toLowerCase().endsWith('.mdx') || path.toLowerCase().endsWith('.markdown');
      
      if (isMd) {
        currentFileContent = data.content;
        mdToggleBtn.style.display = 'block';
        isMarkdownRendered = false; // set to false so toggle turns it to true
        toggleMarkdownView();
      } else {
        const pre = document.createElement('pre');
        pre.style.cssText = "margin:0; padding:16px; font-size:13px; font-family:var(--font-mono); min-height:100%; overflow-x:auto;";
        const code = document.createElement('code');
        code.className = "hljs";
        code.style.cssText = "background:transparent; padding:0;";
        code.textContent = data.content; // textContent prevents XSS
        pre.appendChild(code);

        content.innerHTML = '';
        content.appendChild(pre);

        if (window.hljs) {
          hljs.highlightElement(code);
        }
        addCopyButtons(content);
      }
    } else if (data.type === 'image') {
      content.innerHTML = `<div style="display:flex; justify-content:center; align-items:center; min-height:100%; padding:20px;"><img src="${data.url}" style="max-width:100%; max-height: 70vh; object-fit: contain; border-radius:4px; box-shadow:0 4px 12px rgba(0,0,0,0.5);" /></div>`;
    } else if (data.type === 'video') {
      content.innerHTML = `<div style="display:flex; justify-content:center; align-items:center; min-height:100%; padding:20px;"><video src="${data.url}" controls style="max-width:100%; max-height: 70vh; border-radius:4px; box-shadow:0 4px 12px rgba(0,0,0,0.5);"></video></div>`;
    }
  } catch (err) {
    content.innerHTML = `<div style="color:var(--red); padding: 20px; font-family:var(--font-mono); font-size:13px;">Failed to load file: ${esc(err.message)}</div>`;
  }
}

function closeFileViewer() {
  document.getElementById('file-viewer-panel').style.display = 'none';
  document.getElementById('conversation').style.display = 'flex';
  document.getElementById('fv-content').innerHTML = '';
}
