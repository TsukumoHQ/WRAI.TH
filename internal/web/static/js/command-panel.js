// command-panel.js — Colony Command Panel: agent HUD + management dock
// Replaces the side detail drawer — all agent info lives here now.
// HUD / Retro-futurism aesthetic: scanlines, neon glow, monospace

function esc(str) {
  // Quote-safe: also escapes " and ' so values are safe inside HTML attributes
  // (textContent→innerHTML does NOT escape quotes → attribute-injection XSS).
  if (str == null) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function timeAgo(dateStr) {
  if (!dateStr) return '';
  const diff = Date.now() - new Date(dateStr).getTime();
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

const CMD_STYLES = `
/* ══════════════════════════════════════════════════════════
   COMMAND PANEL — HUD / Retro-Futurism Console
   ══════════════════════════════════════════════════════════ */

@keyframes cmdFadeIn {
  from { opacity: 0; transform: translateY(6px); }
  to   { opacity: 1; transform: translateY(0); }
}
@keyframes cmdPulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}
@keyframes scanline {
  0% { background-position: 0 0; }
  100% { background-position: 0 4px; }
}

.cmd-root {
  font-family: 'JetBrains Mono', monospace;
  color: #c8ccd4;
  width: 100%; height: 100%;
  display: flex; flex-direction: column;
  overflow: hidden;
  position: relative;
}
/* Subtle scanline overlay */
.cmd-root::before {
  content: '';
  position: absolute; inset: 0;
  background: repeating-linear-gradient(
    0deg,
    transparent,
    transparent 2px,
    rgba(108, 92, 231, 0.02) 2px,
    rgba(108, 92, 231, 0.02) 4px
  );
  pointer-events: none;
  z-index: 1;
}

/* ── CREATION BAR (no agent) ── */
.cmd-creation-bar {
  display: flex; gap: 12px;
  padding: 16px 24px;
  align-items: center;
  flex-shrink: 0;
}
.cmd-creation-bar::before {
  content: 'COMMAND';
  color: rgba(108, 92, 231, 0.5);
  font-size: 9px;
  font-weight: 700;
  letter-spacing: 2px;
  margin-right: 8px;
}
.cmd-create-btn {
  padding: 10px 20px;
  font-size: 10px; font-weight: 700;
  letter-spacing: 1.5px;
  border: 1px solid rgba(108, 92, 231, 0.4);
  background: rgba(108, 92, 231, 0.08);
  color: #a29bfe;
  cursor: pointer;
  border-radius: 3px;
  transition: all 0.2s ease;
  font-family: inherit;
  text-transform: uppercase;
}
.cmd-create-btn:hover {
  background: rgba(108, 92, 231, 0.15);
  border-color: rgba(108, 92, 231, 0.6);
  color: #a29bfe;
  box-shadow: 0 0 12px rgba(108, 92, 231, 0.15);
}
.cmd-create-btn.active {
  background: rgba(108, 92, 231, 0.2);
  border-color: #a29bfe;
  color: #d4cfff;
  box-shadow: 0 0 16px rgba(108, 92, 231, 0.25), inset 0 0 8px rgba(108, 92, 231, 0.1);
}

.cmd-form-area {
  flex: 1; overflow-y: auto;
  padding: 12px 20px;
  animation: cmdFadeIn 0.2s ease;
}

/* ── AGENT HUD — horizontal layout ── */
.cmd-agent-layout {
  display: flex;
  flex: 1;
  overflow: hidden;
  min-height: 0;
}

/* Left: Identity + live data */
.cmd-hud-left {
  width: 280px;
  min-width: 240px;
  flex-shrink: 0;
  border-right: 1px solid rgba(108, 92, 231, 0.15);
  display: flex;
  flex-direction: column;
  overflow-y: auto;
  padding: 12px 16px;
}

/* Right: Tabbed management */
.cmd-hud-right {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  min-width: 0;
}

/* ── HUD Identity Block ── */
.cmd-hero {
  margin-bottom: 10px;
}
.cmd-hero-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.cmd-hero-name {
  font-size: 13px;
  font-weight: 700;
  letter-spacing: 1px;
  color: #e0e0e8;
  text-shadow: 0 0 8px rgba(162, 155, 254, 0.3);
}
.cmd-hero-status {
  font-size: 8px;
  font-weight: 700;
  letter-spacing: 1px;
  padding: 2px 8px;
  border-radius: 2px;
  margin-left: auto;
}
.cmd-hero-status.online {
  color: #00e676;
  background: rgba(0, 230, 118, 0.08);
  border: 1px solid rgba(0, 230, 118, 0.25);
  box-shadow: 0 0 6px rgba(0, 230, 118, 0.15);
}
.cmd-hero-status.offline {
  color: #636e72;
  background: rgba(99, 110, 114, 0.06);
  border: 1px solid rgba(99, 110, 114, 0.2);
}
.cmd-hero-status.sleeping {
  color: #9b59b6;
  background: rgba(155, 89, 182, 0.06);
  border: 1px solid rgba(155, 89, 182, 0.2);
}
.cmd-hero-role {
  font-size: 9px;
  color: #7f8694;
  letter-spacing: 0.5px;
  margin-bottom: 2px;
}
.cmd-hero-activity {
  font-size: 9px;
  color: #a29bfe;
  animation: cmdPulse 2s ease infinite;
}

/* ── HUD Stats Grid ── */
.cmd-stats {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 4px;
  margin: 8px 0;
}
.cmd-stat {
  padding: 5px 8px;
  background: rgba(15, 15, 30, 0.5);
  border: 1px solid rgba(108, 92, 231, 0.08);
  border-radius: 2px;
}
.cmd-stat-value {
  font-size: 11px;
  font-weight: 700;
  color: #a29bfe;
  display: block;
}
.cmd-stat-label {
  font-size: 7px;
  color: #5a5e78;
  letter-spacing: 1px;
  text-transform: uppercase;
}

/* ── Current Task ── */
.cmd-current-task {
  padding: 8px 10px;
  background: rgba(0, 230, 118, 0.03);
  border-left: 2px solid rgba(0, 230, 118, 0.4);
  border-radius: 0 2px 2px 0;
  margin: 6px 0;
}
.cmd-current-task-label {
  font-size: 7px;
  color: #5a5e78;
  letter-spacing: 1.5px;
  text-transform: uppercase;
  margin-bottom: 3px;
}
.cmd-current-task-title {
  font-size: 10px;
  color: #dfe6e9;
}
.cmd-current-task-status {
  font-size: 8px;
  margin-top: 2px;
}

/* ── Hierarchy / Teams compact ── */
.cmd-hud-section {
  margin-top: 8px;
  padding-top: 6px;
  border-top: 1px solid rgba(108, 92, 231, 0.08);
}
.cmd-hud-section-label {
  font-size: 7px;
  color: #5a5e78;
  letter-spacing: 1.5px;
  text-transform: uppercase;
  margin-bottom: 4px;
}
.cmd-tag {
  display: inline-block;
  padding: 2px 7px;
  font-size: 8px;
  font-weight: 600;
  background: rgba(108, 92, 231, 0.08);
  border: 1px solid rgba(108, 92, 231, 0.15);
  border-radius: 2px;
  color: #a29bfe;
  margin: 0 3px 3px 0;
  cursor: pointer;
  transition: all 0.15s;
}
.cmd-tag:hover {
  background: rgba(108, 92, 231, 0.2);
  border-color: rgba(108, 92, 231, 0.4);
}
.cmd-tag.gold {
  color: #f6c243;
  border-color: rgba(246, 194, 67, 0.25);
  background: rgba(246, 194, 67, 0.06);
}
.cmd-tag.dim {
  color: #636e72;
  cursor: default;
}
.cmd-tag.dim:hover { background: rgba(108, 92, 231, 0.08); }

/* ── Recent Comms compact ── */
.cmd-msg-item {
  display: flex;
  align-items: baseline;
  gap: 5px;
  padding: 3px 0;
  font-size: 9px;
  color: #7f8694;
  border-bottom: 1px solid rgba(108, 92, 231, 0.04);
}
.cmd-msg-item:last-child { border-bottom: none; }
.cmd-msg-dir { font-weight: 700; font-size: 10px; flex-shrink: 0; }
.cmd-msg-peer { color: #a29bfe; flex-shrink: 0; }
.cmd-msg-preview {
  flex: 1; min-width: 0;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  color: #636e72;
}
.cmd-msg-time { flex-shrink: 0; color: #3d4150; font-size: 8px; }

/* ── Nav arrows ── */
.cmd-nav {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: auto;
  padding-top: 8px;
  border-top: 1px solid rgba(108, 92, 231, 0.08);
}
.cmd-nav-btn {
  background: none; border: 1px solid rgba(108, 92, 231, 0.2);
  color: #7c6fe0; cursor: pointer;
  width: 24px; height: 24px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 2px; font-size: 12px;
  font-family: inherit;
  transition: all 0.15s;
}
.cmd-nav-btn:hover {
  background: rgba(108, 92, 231, 0.15);
  border-color: rgba(108, 92, 231, 0.5);
}
.cmd-nav-label {
  font-size: 8px; color: #5a5e78;
  letter-spacing: 1px; flex: 1; text-align: center;
}

/* ── TAB BAR ── */
.cmd-tabs {
  display: flex; gap: 0;
  border-bottom: 1px solid rgba(108, 92, 231, 0.15);
  flex-shrink: 0;
  padding: 0 16px;
  background: rgba(10, 10, 26, 0.5);
}
.cmd-tab {
  padding: 8px 14px;
  font-size: 8px; font-weight: 700;
  letter-spacing: 1.5px;
  color: #4a4e64;
  cursor: pointer; border: none; background: none;
  position: relative;
  transition: color 0.15s;
  font-family: inherit;
  text-transform: uppercase;
}
.cmd-tab:hover { color: #7c6fe0; }
.cmd-tab.active { color: #a29bfe; }
.cmd-tab.active::after {
  content: '';
  position: absolute;
  bottom: -1px; left: 8px; right: 8px;
  height: 2px;
  background: linear-gradient(90deg, transparent, #a29bfe, transparent);
  box-shadow: 0 0 6px rgba(162, 155, 254, 0.4);
}

/* ── TAB CONTENT ── */
.cmd-tab-content {
  flex: 1; overflow-y: auto;
  padding: 12px 16px;
  animation: cmdFadeIn 0.15s ease;
}

/* ── ACTIONS BAR ── */
.cmd-actions {
  display: flex; gap: 6px;
  padding: 8px 16px;
  border-top: 1px solid rgba(108, 92, 231, 0.12);
  flex-shrink: 0;
  align-items: center;
  background: rgba(10, 10, 26, 0.4);
}

/* ── Shared: forms, buttons, lists ── */
.cmd-form { display: flex; flex-direction: column; gap: 8px; }
.cmd-form-row { display: flex; gap: 8px; align-items: center; }
.cmd-form-row.wide { flex-direction: column; align-items: stretch; }
.cmd-label {
  font-size: 8px; font-weight: 700;
  letter-spacing: 1.5px; color: #4a4e64;
  min-width: 80px; flex-shrink: 0;
  text-transform: uppercase;
}
.cmd-input, .cmd-select, .cmd-textarea {
  font-family: inherit;
  font-size: 10px; padding: 5px 8px;
  background: rgba(10, 10, 26, 0.7);
  border: 1px solid rgba(108, 92, 231, 0.18);
  color: #c8ccd4;
  border-radius: 2px; flex: 1; min-width: 0;
  transition: border-color 0.15s, box-shadow 0.15s;
}
.cmd-input:focus, .cmd-select:focus, .cmd-textarea:focus {
  outline: none;
  border-color: rgba(108, 92, 231, 0.5);
  box-shadow: 0 0 8px rgba(108, 92, 231, 0.15);
}
.cmd-textarea { resize: vertical; min-height: 40px; }
.cmd-input[readonly] { opacity: 0.4; cursor: not-allowed; }

.cmd-btn {
  padding: 5px 12px;
  font-size: 8px; font-weight: 700;
  letter-spacing: 1px;
  border: 1px solid rgba(108, 92, 231, 0.3);
  background: rgba(108, 92, 231, 0.06);
  color: #7c6fe0;
  cursor: pointer; border-radius: 2px;
  transition: all 0.2s ease;
  font-family: inherit;
  text-transform: uppercase;
}
.cmd-btn:hover {
  background: rgba(108, 92, 231, 0.18);
  border-color: #a29bfe;
  box-shadow: 0 0 8px rgba(108, 92, 231, 0.15);
}
.cmd-btn:active { transform: scale(0.97); }
.cmd-btn.primary {
  background: rgba(108, 92, 231, 0.2);
  border-color: #a29bfe;
  color: #d4cfff;
}
.cmd-btn.primary:hover {
  background: rgba(108, 92, 231, 0.35);
  box-shadow: 0 0 12px rgba(108, 92, 231, 0.25);
}
.cmd-btn.danger {
  border-color: rgba(255, 107, 107, 0.3);
  color: #e74c3c;
  background: rgba(255, 107, 107, 0.04);
}
.cmd-btn.danger:hover {
  background: rgba(255, 107, 107, 0.15);
  border-color: #ff6b6b;
  box-shadow: 0 0 8px rgba(255, 107, 107, 0.15);
}
.cmd-btn.success {
  border-color: rgba(0, 230, 118, 0.3);
  color: #00c853;
  background: rgba(0, 230, 118, 0.04);
}
.cmd-btn.success:hover {
  background: rgba(0, 230, 118, 0.15);
  border-color: #00e676;
  box-shadow: 0 0 8px rgba(0, 230, 118, 0.15);
}

/* ── List items ── */
.cmd-list-item {
  display: flex; align-items: center;
  gap: 10px; padding: 5px 0;
  border-bottom: 1px solid rgba(108, 92, 231, 0.06);
  font-size: 10px;
}
.cmd-list-item:last-child { border-bottom: none; }
.cmd-list-name { flex: 1; color: #c8ccd4; }
.cmd-list-meta { color: #4a4e64; font-size: 9px; }
.cmd-list-actions { display: flex; gap: 4px; }

.cmd-inline-form {
  padding: 10px;
  background: rgba(10, 10, 26, 0.6);
  border: 1px solid rgba(108, 92, 231, 0.12);
  border-radius: 2px; margin-top: 8px;
  animation: cmdFadeIn 0.15s ease;
}

.cmd-empty {
  text-align: center; color: #3d4150;
  font-size: 9px; padding: 16px;
  letter-spacing: 1px;
}

/* Terminal tab */
#cmd-xterm { flex: 1; min-height: 0; }
#cmd-xterm .xterm { height: 100%; }
#cmd-xterm .xterm-viewport { overflow-y: auto !important; }
.cmd-tab-content:has(#cmd-xterm) { padding: 0; }

/* ── Send message form ── */
.cmd-send-form {
  display: flex; flex-direction: column; gap: 6px;
  padding: 8px 16px;
  border-top: 1px solid rgba(108, 92, 231, 0.1);
  background: rgba(10, 10, 26, 0.4);
  flex-shrink: 0;
  animation: cmdFadeIn 0.15s ease;
}
.cmd-send-row { display: flex; gap: 6px; align-items: center; }
`;

export class CommandPanel {
  constructor(container, resizeHandle) {
    this._container = container;
    this._resizeHandle = resizeHandle;
    this._project = null;
    this._agent = null;
    this._activeTab = 'profile';
    this._client = null;
    this._profiles = [];
    this._cycles = [];
    this._activeCreateForm = null;
    this._msgFormOpen = false;
    this._onNavigate = null; // callback(agentKey) for prev/next
    this._terminal = null;     // xterm.js Terminal instance
    this._termWs = null;       // WebSocket connection
    this._termSessionId = null; // PTY session ID
    this._termBox = null;      // persistent DOM container for xterm (survives tab switches)
    this._termFit = null;      // FitAddon instance
    this._termByAgent = {};    // slug → sessionId (survives agent switches)

    if (!document.getElementById('cmd-panel-styles')) {
      const style = document.createElement('style');
      style.id = 'cmd-panel-styles';
      style.textContent = CMD_STYLES;
      document.head.appendChild(style);
    }
    this._initResize();
  }

  setClient(client) { this._client = client; }

  /** @param {Function} fn - called with direction: -1 or +1 */
  set onNavigate(fn) { this._onNavigate = fn; }

  show(project) {
    this._project = project;
    this._loadCacheLists();
    if (!this._agent) this._renderCreationBar();
  }

  hide() {
    // Visibility is controlled by body.view-galaxy .colony-only { display: none !important }
    // No need to set inline styles
    this._resizeHandle.style.display = 'none';
  }

  setAgent(agentData) {
    const prevSlug = this._agent?.slug || this._agent?.name;
    const newSlug = agentData?.slug || agentData?.name;
    // Switching to a different agent — save session ID, teardown UI only
    if (prevSlug && newSlug && prevSlug !== newSlug) {
      if (this._termSessionId) {
        this._termByAgent[prevSlug] = this._termSessionId;
      }
      this._teardownTerminalUI();
      // Restore session ID if this agent had one running
      this._termSessionId = this._termByAgent[newSlug] || null;
    }
    this._agent = agentData;
    if (!this._agent._tabVisited) this._activeTab = 'profile';
    this._agent._tabVisited = true;
    this._msgFormOpen = false;
    this._renderAgentPanel();
  }

  clearAgent() {
    // Save session before clearing
    const slug = this._agent?.slug || this._agent?.name;
    if (slug && this._termSessionId) {
      this._termByAgent[slug] = this._termSessionId;
    }
    this._agent = null;
    this._activeCreateForm = null;
    this._msgFormOpen = false;
    this._teardownTerminalUI();
    this._termSessionId = null;
    this._renderCreationBar();
  }

  // Teardown xterm UI only (WS + DOM). PTY session stays alive on server.
  _teardownTerminalUI() {
    if (this._termWs) { this._termWs.close(); this._termWs = null; }
    if (this._terminal) { this._terminal.dispose(); this._terminal = null; }
    if (this._termBox) { this._termBox.remove(); this._termBox = null; }
    this._termFit = null;
  }

  // Full destroy: teardown UI + forget session ID
  _destroyTerminal() {
    const slug = this._agent?.slug || this._agent?.name;
    if (slug) delete this._termByAgent[slug];
    this._teardownTerminalUI();
    this._termSessionId = null;
  }

  // ── Internals ──

  async _loadCacheLists() {
    if (!this._client || !this._project) return;
    try {
      this._profiles = await this._client.fetchProfiles(this._project);
    } catch { /* ignore */ }
  }

  // ════════════════════════════════════════════
  //  CREATION BAR (no agent selected)
  // ════════════════════════════════════════════

  _renderCreationBar() {
    this._container.innerHTML = '';
    const root = document.createElement('div');
    root.className = 'cmd-root';
    const hint = document.createElement('div');
    hint.className = 'cmd-empty-hint';
    hint.textContent = 'Select an agent to inspect it. Agents register themselves via the relay.';
    root.appendChild(hint);
    this._container.appendChild(root);
  }

  // ════════════════════════════════════════════
  //  AGENT HUD PANEL
  // ════════════════════════════════════════════

  _renderAgentPanel() {
    const a = this._agent;
    if (!a) return;
    // Detach the persistent terminal box before clearing (so it isn't destroyed)
    if (this._termBox && this._termBox.parentNode) {
      this._termBox.remove();
    }
    this._container.innerHTML = '';
    const root = document.createElement('div');
    root.className = 'cmd-root';

    const layout = document.createElement('div');
    layout.className = 'cmd-agent-layout';

    // ── LEFT: Identity HUD ──
    const left = document.createElement('div');
    left.className = 'cmd-hud-left';
    this._renderHudLeft(left, a);
    layout.appendChild(left);

    // ── RIGHT: Tabbed management ──
    const right = document.createElement('div');
    right.className = 'cmd-hud-right';

    // Tab bar
    const tabs = document.createElement('div');
    tabs.className = 'cmd-tabs';
    const tabDefs = ['profile', 'comms', 'tasks'];
    for (const t of tabDefs) {
      const btn = document.createElement('button');
      btn.className = 'cmd-tab' + (this._activeTab === t ? ' active' : '');
      btn.textContent = t.charAt(0).toUpperCase() + t.slice(1);
      btn.addEventListener('click', () => { this._activeTab = t; this._renderAgentPanel(); });
      tabs.appendChild(btn);
    }
    right.appendChild(tabs);

    // Tab content
    const content = document.createElement('div');
    content.className = 'cmd-tab-content';
    if (this._activeTab === 'profile') this._renderProfileTab(content);
    else if (this._activeTab === 'comms') this._renderCommsTab(content);
    else if (this._activeTab === 'tasks') this._renderTasksTab(content);
    right.appendChild(content);

    // Actions bar
    const actions = document.createElement('div');
    actions.className = 'cmd-actions';

    const msgBtn = document.createElement('button');
    msgBtn.className = 'cmd-btn';
    msgBtn.textContent = 'Message';
    msgBtn.addEventListener('click', () => { this._msgFormOpen = !this._msgFormOpen; this._renderAgentPanel(); });
    actions.appendChild(msgBtn);

    right.appendChild(actions);

    // Message form (inline, below actions)
    if (this._msgFormOpen) {
      const mf = document.createElement('div');
      mf.className = 'cmd-send-form';
      mf.innerHTML = `
        <div class="cmd-send-row">
          <span class="cmd-label">TO</span>
          <input class="cmd-input" data-field="to" value="${esc(a.slug || a.name)}" />
          <span class="cmd-label" style="min-width:auto">PRI</span>
          <select class="cmd-select" data-field="priority" style="width:70px;flex:none">
            <option value="normal">normal</option><option value="high">high</option><option value="low">low</option>
          </select>
        </div>
        <div class="cmd-send-row">
          <textarea class="cmd-textarea" data-field="content" rows="2" placeholder="Message..." style="flex:1"></textarea>
          <button class="cmd-btn primary" id="cmd-send-msg-btn" style="align-self:flex-end">Send</button>
        </div>
      `;
      right.appendChild(mf);
      mf.querySelector('#cmd-send-msg-btn').addEventListener('click', () => this._doSendMessage(mf));
    }

    layout.appendChild(right);
    root.appendChild(layout);
    this._container.appendChild(root);
  }

  _renderHudLeft(left, a) {
    // Hero identity
    const hero = document.createElement('div');
    hero.className = 'cmd-hero';

    const statusClass = a.sleeping ? 'sleeping' : a.online ? 'online' : 'offline';
    const statusText = a.sleeping ? 'ZZZ' : a.online ? 'ONLINE' : 'OFFLINE';

    hero.innerHTML = `
      <div class="cmd-hero-row">
        <span class="cmd-hero-name" style="color:${a.color || '#a29bfe'}">${esc(a.name || a.slug)}</span>
        <span class="cmd-hero-status ${statusClass}">${statusText}</span>
      </div>
      <div class="cmd-hero-role">${esc(a.role || '')}</div>
      ${a.activity && a.activity !== 'idle' ? `<div class="cmd-hero-activity">${esc(a.activityTool || a.activity)}</div>` : ''}
    `;
    left.appendChild(hero);

    // Stats grid
    const stats = document.createElement('div');
    stats.className = 'cmd-stats';
    const lastSeen = a._lastSeenRaw ? timeAgo(a._lastSeenRaw) : '--';
    const taskCount = (a._tasks || []).length;
    stats.innerHTML = `
      <div class="cmd-stat"><span class="cmd-stat-value">${lastSeen}</span><span class="cmd-stat-label">Last seen</span></div>
      <div class="cmd-stat"><span class="cmd-stat-value">${taskCount}</span><span class="cmd-stat-label">Tasks</span></div>
    `;
    left.appendChild(stats);

    // Current task
    const tasks = a._tasks || [];
    const current = tasks.find(t => t.status === 'in-progress') || tasks[0];
    if (current) {
      const statusColors = { pending: '#ffd93d', accepted: '#74b9ff', 'in-progress': '#00e676', blocked: '#ff6b6b' };
      const c = statusColors[current.status] || '#636e72';
      const ct = document.createElement('div');
      ct.className = 'cmd-current-task';
      ct.innerHTML = `
        <div class="cmd-current-task-label">Current Task</div>
        <div class="cmd-current-task-title">${esc(current.title)}</div>
        <div class="cmd-current-task-status" style="color:${c}">${current.status} &middot; ${current.priority}</div>
      `;
      left.appendChild(ct);
    }

    // Hierarchy
    if (a._reportsTo || (a._directReports && a._directReports.length)) {
      const sec = document.createElement('div');
      sec.className = 'cmd-hud-section';
      sec.innerHTML = '<div class="cmd-hud-section-label">Hierarchy</div>';
      if (a._reportsTo) {
        const tag = document.createElement('span');
        tag.className = 'cmd-tag';
        tag.textContent = '\u25B2 ' + a._reportsTo;
        tag.addEventListener('click', () => { if (this._onNavigate) this._onNavigate(a._reportsTo); });
        sec.appendChild(tag);
      }
      for (const r of (a._directReports || [])) {
        const tag = document.createElement('span');
        tag.className = 'cmd-tag';
        tag.textContent = '\u25BC ' + r;
        tag.addEventListener('click', () => { if (this._onNavigate) this._onNavigate(r); });
        sec.appendChild(tag);
      }
      left.appendChild(sec);
    }

    // Teams
    if (a._teams && a._teams.length) {
      const sec = document.createElement('div');
      sec.className = 'cmd-hud-section';
      sec.innerHTML = '<div class="cmd-hud-section-label">Teams</div>';
      for (const t of a._teams) {
        const tag = document.createElement('span');
        tag.className = 'cmd-tag' + (t.type === 'admin' ? ' gold' : '');
        tag.textContent = (t.type === 'admin' ? '\u2605 ' : '') + t.name;
        tag.title = `Role: ${t.role || '-'} | Type: ${t.type}`;
        sec.appendChild(tag);
      }
      left.appendChild(sec);
    }

    // Nav arrows
    const nav = document.createElement('div');
    nav.className = 'cmd-nav';
    const prevBtn = document.createElement('button');
    prevBtn.className = 'cmd-nav-btn';
    prevBtn.textContent = '\u25C0';
    prevBtn.title = 'Previous agent';
    prevBtn.addEventListener('click', () => { if (this._onNavigate) this._onNavigate(-1); });
    const nextBtn = document.createElement('button');
    nextBtn.className = 'cmd-nav-btn';
    nextBtn.textContent = '\u25B6';
    nextBtn.title = 'Next agent';
    nextBtn.addEventListener('click', () => { if (this._onNavigate) this._onNavigate(+1); });
    const navLabel = document.createElement('span');
    navLabel.className = 'cmd-nav-label';
    navLabel.textContent = a._navLabel || '';
    nav.appendChild(prevBtn);
    nav.appendChild(navLabel);
    nav.appendChild(nextBtn);
    left.appendChild(nav);
  }

  // ── Tab renderers ──

  _renderProfileTab(content) {
    const a = this._agent;
    const form = document.createElement('div');
    form.className = 'cmd-form';
    form.innerHTML = `
      <div class="cmd-form-row">
        <span class="cmd-label">SLUG</span>
        <input class="cmd-input" value="${esc(a.slug || '')}" readonly />
      </div>
      <div class="cmd-form-row">
        <span class="cmd-label">NAME</span>
        <input class="cmd-input" value="${esc(a.name || '')}" readonly />
      </div>
      <div class="cmd-form-row">
        <span class="cmd-label">ROLE</span>
        <input class="cmd-input" value="${esc(a.role || '')}" readonly />
      </div>
    `;
    content.appendChild(form);
  }

  _renderCommsTab(content) {
    const msgs = this._agent._recentMsgs || [];
    if (msgs.length === 0) {
      content.innerHTML = '<div class="cmd-empty">No recent messages</div>';
      return;
    }
    for (const m of msgs) {
      const isSent = m.from === (this._agent.slug || this._agent.name);
      const peer = isSent ? (m.to || 'broadcast') : m.from;
      const dir = isSent ? '\u2192' : '\u2190';
      const dirColor = isSent ? '#a29bfe' : '#74b9ff';
      const preview = m.content.length > 80 ? m.content.slice(0, 78) + '...' : m.content;
      const item = document.createElement('div');
      item.className = 'cmd-msg-item';
      item.innerHTML = `
        <span class="cmd-msg-dir" style="color:${dirColor}">${dir}</span>
        <span class="cmd-msg-peer">${esc(peer)}</span>
        <span class="cmd-msg-preview">${esc(preview)}</span>
        <span class="cmd-msg-time">${timeAgo(m.created_at)}</span>
      `;
      content.appendChild(item);
    }
  }

  _renderTasksTab(content) {
    const tasks = this._agent._tasks || [];
    if (tasks.length === 0) {
      content.innerHTML = '<div class="cmd-empty">No active tasks</div>';
      return;
    }
    const statusColors = { pending: '#ffd93d', accepted: '#74b9ff', 'in-progress': '#00e676', blocked: '#ff6b6b' };
    for (const t of tasks) {
      const c = statusColors[t.status] || '#636e72';
      const item = document.createElement('div');
      item.className = 'cmd-list-item';
      item.innerHTML = `
        <span style="color:${c};font-size:9px;font-weight:700;min-width:70px">${t.status}</span>
        <span class="cmd-list-name">${esc(t.title.length > 50 ? t.title.slice(0, 48) + '...' : t.title)}</span>
        <span class="cmd-list-meta">${t.priority}</span>
      `;
      content.appendChild(item);
    }
  }

  async _doSendMessage(form) {
    const get = (f) => form.querySelector(`[data-field="${f}"]`)?.value?.trim() || '';
    const to = get('to');
    const content = get('content');
    if (!to || !content) return;
    await this._client.sendUserResponse(this._project, to, content, null);
    this._msgFormOpen = false;
    this._renderAgentPanel();
  }

  // ── Resize / collapse ──
  // Drag the grab bar = resize. Plain click (< 4px movement) = collapse to
  // just the bar / expand back. Height + collapsed state persist.

  _initResize() {
    let startY = 0, startH = 0, active = false, moved = false;

    const savedH = parseInt(localStorage.getItem('cmdDockHeight'), 10);
    if (savedH >= 120) this._container.style.height = savedH + 'px';
    if (localStorage.getItem('cmdDockCollapsed') === '1') this._setCollapsed(true);

    const onMove = (e) => {
      if (!active) return;
      const dy = startY - e.clientY;
      if (Math.abs(dy) > 4) moved = true;
      if (!moved) return;
      if (this._collapsed) this._setCollapsed(false);
      const newH = Math.max(120, Math.min(window.innerHeight * 0.6, startH + dy));
      this._container.style.height = newH + 'px';
      // No resize dispatch: the dock floats over the canvas stage, so its
      // height never reflows the scene.
    };
    const onUp = () => {
      if (!active) return;
      active = false;
      document.body.style.userSelect = '';
      document.body.style.cursor = '';
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
      if (moved) {
        localStorage.setItem('cmdDockHeight', String(this._container.offsetHeight));
      } else {
        this._setCollapsed(!this._collapsed);
      }
    };
    this._resizeHandle.addEventListener('mousedown', (e) => {
      e.preventDefault();
      active = true;
      moved = false;
      startY = e.clientY;
      startH = this._collapsed
        ? (parseInt(localStorage.getItem('cmdDockHeight'), 10) || 200)
        : this._container.offsetHeight;
      document.body.style.userSelect = 'none';
      document.body.style.cursor = 'ns-resize';
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
    this._resizeHandle.title = 'Glisser : redimensionner · Clic : replier/déplier';
  }

  _setCollapsed(collapsed) {
    this._collapsed = collapsed;
    this._container.classList.toggle('dock-collapsed', collapsed);
    this._resizeHandle.classList.toggle('dock-collapsed', collapsed);
    localStorage.setItem('cmdDockCollapsed', collapsed ? '1' : '0');
  }
}
