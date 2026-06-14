// notifications.js — Notifications rules panel (event → action → target)
// Configurable rules engine UI. Dark theme, no framework, vanilla DOM.

const EVENTS = [
  "task.dispatched",
  "task.claimed",
  "task.in_progress",
  "task.blocked",
  "task.in_review",
  "task.done",
  "cycle.digest",
];

const ACTIONS = ["message", "webhook", "slack"];

function esc(s) {
  // Quote-safe (escapes " and ') for safe use inside HTML attributes.
  if (s == null) return "";
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function timeAgo(dateStr) {
  if (!dateStr) return "";
  const diff = Date.now() - new Date(dateStr).getTime();
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

const STYLES = `
#notifications-panel.nf-active { display: flex; flex-direction: column; }
.nf-wrap { display:flex; flex-direction:column; height:100%; color:#cdd6f4; font-family:'SF Mono',ui-monospace,Menlo,monospace; overflow:hidden; }
.nf-header { display:flex; align-items:center; justify-content:space-between; padding:14px 20px; border-bottom:1px solid #2a2a3a; }
.nf-title { font-size:15px; font-weight:600; letter-spacing:.04em; color:#a6e3a1; }
.nf-sub { font-size:11px; color:#7f849c; margin-top:2px; }
.nf-tabs { display:flex; gap:6px; }
.nf-tab { background:#1a1a26; border:1px solid #2a2a3a; color:#9399b2; padding:6px 14px; font-size:12px; cursor:pointer; border-radius:4px; }
.nf-tab.active { background:#2a2a3a; color:#cdd6f4; border-color:#45475a; }
.nf-body { flex:1; overflow-y:auto; padding:16px 20px; }
.nf-toolbar { display:flex; justify-content:flex-end; margin-bottom:12px; }
.nf-btn { background:#313244; border:1px solid #45475a; color:#cdd6f4; padding:7px 14px; font-size:12px; cursor:pointer; border-radius:4px; font-family:inherit; }
.nf-btn:hover { background:#45475a; }
.nf-btn-primary { background:#74c7ec; color:#11111b; border-color:#74c7ec; font-weight:600; }
.nf-btn-primary:hover { background:#89dceb; }
.nf-btn-danger { color:#f38ba8; border-color:#5a2a35; }
.nf-btn-sm { padding:4px 9px; font-size:11px; }
.nf-list-head, .nf-rule { display:grid; grid-template-columns:46px 1.4fr 1fr 1.4fr 1.2fr 130px; gap:10px; align-items:center; }
.nf-list-head { font-size:10px; text-transform:uppercase; letter-spacing:.08em; color:#6c7086; padding:0 12px 8px; }
.nf-rule { background:#181825; border:1px solid #262636; border-radius:6px; padding:11px 12px; margin-bottom:8px; }
.nf-rule.disabled { opacity:.5; }
.nf-rule-name { font-size:13px; color:#cdd6f4; font-weight:500; }
.nf-rule-name .nf-match { display:block; font-size:10px; color:#6c7086; margin-top:2px; }
.nf-pill { display:inline-block; font-size:10px; padding:2px 8px; border-radius:10px; background:#2a2a3a; color:#bac2de; }
.nf-pill.event { background:#1e2a3a; color:#89b4fa; }
.nf-pill.action-webhook { background:#2a1e3a; color:#cba6f7; }
.nf-pill.action-message { background:#1e3a2a; color:#a6e3a1; }
.nf-pill.action-slack { background:#3a2a1e; color:#fab387; }
.nf-target { font-size:12px; color:#bac2de; word-break:break-all; }
.nf-target.empty { color:#f9e2af; font-style:italic; }
.nf-actions { display:flex; gap:6px; justify-content:flex-end; }
.nf-toggle { position:relative; width:38px; height:20px; background:#45475a; border-radius:10px; cursor:pointer; transition:background .15s; flex-shrink:0; }
.nf-toggle.on { background:#a6e3a1; }
.nf-toggle::after { content:''; position:absolute; top:2px; left:2px; width:16px; height:16px; background:#fff; border-radius:50%; transition:left .15s; }
.nf-toggle.on::after { left:20px; }
.nf-empty { text-align:center; color:#6c7086; padding:40px; font-size:13px; }
/* deliveries */
.nf-del { display:grid; grid-template-columns:80px 110px 90px 1fr 90px; gap:10px; align-items:center; background:#181825; border:1px solid #262636; border-radius:5px; padding:9px 12px; margin-bottom:6px; font-size:12px; }
.nf-del-head { grid-template-columns:80px 110px 90px 1fr 90px; display:grid; gap:10px; font-size:10px; text-transform:uppercase; color:#6c7086; padding:0 12px 8px; }
.nf-outcome { font-weight:600; }
.nf-outcome.ok { color:#a6e3a1; }
.nf-outcome.failed { color:#f38ba8; }
.nf-outcome.dryrun { color:#f9e2af; }
.nf-del-line { color:#bac2de; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
/* editor modal */
.nf-modal { position:fixed; inset:0; background:rgba(0,0,0,.6); display:flex; align-items:center; justify-content:center; z-index:1000; }
.nf-modal-box { background:#11111b; border:1px solid #313244; border-radius:8px; width:520px; max-width:92vw; max-height:88vh; overflow-y:auto; padding:22px; }
.nf-modal-title { font-size:15px; color:#a6e3a1; margin-bottom:16px; font-weight:600; }
.nf-field { margin-bottom:13px; }
.nf-field label { display:block; font-size:11px; color:#9399b2; margin-bottom:5px; text-transform:uppercase; letter-spacing:.05em; }
.nf-field input, .nf-field select, .nf-field textarea { width:100%; background:#1a1a26; border:1px solid #313244; color:#cdd6f4; padding:8px 10px; font-size:13px; border-radius:4px; font-family:inherit; box-sizing:border-box; }
.nf-field textarea { resize:vertical; min-height:54px; }
.nf-field .nf-hint { font-size:10px; color:#6c7086; margin-top:4px; }
.nf-modal-actions { display:flex; justify-content:space-between; margin-top:20px; }
.nf-testbox { background:#0d0d16; border:1px solid #262636; border-radius:5px; padding:10px; margin-top:10px; font-size:11px; color:#a6adc8; white-space:pre-wrap; word-break:break-all; max-height:160px; overflow-y:auto; }
`;

export class NotificationsPanel {
  constructor(container) {
    this.el = container;
    this.project = "default";
    this.tab = "rules";
    this.rules = [];
    this.deliveries = [];
    this.customEvents = [];
    this._injectStyles();
    this._renderShell();
  }

  _injectStyles() {
    if (document.getElementById("nf-styles")) return;
    const s = document.createElement("style");
    s.id = "nf-styles";
    s.textContent = STYLES;
    document.head.appendChild(s);
  }

  show(project) {
    if (project) this.project = project;
    this.el.classList.remove("hidden");
    this.el.classList.add("nf-active");
    this.refresh();
  }

  hide() {
    this.el.classList.add("hidden");
    this.el.classList.remove("nf-active");
  }

  async refresh() {
    await Promise.all([this._loadRules(), this._loadDeliveries(), this._loadCustomEvents()]);
    this._renderBody();
  }

  async _loadCustomEvents() {
    try {
      const res = await fetch(`/api/custom-events?project=${encodeURIComponent(this.project)}`);
      const evts = res.ok ? await res.json() : [];
      this.customEvents = (evts || []).map(e => `event:${e.name}`);
    } catch { this.customEvents = []; }
  }

  _allEvents() {
    return [...EVENTS, ...this.customEvents];
  }

  async _loadRules() {
    try {
      const res = await fetch(`/api/notification-rules?project=${encodeURIComponent(this.project)}`);
      this.rules = res.ok ? await res.json() : [];
    } catch { this.rules = []; }
  }

  async _loadDeliveries() {
    try {
      const res = await fetch(`/api/notification-deliveries?limit=100`);
      this.deliveries = res.ok ? await res.json() : [];
    } catch { this.deliveries = []; }
  }

  _renderShell() {
    this.el.innerHTML = `
      <div class="nf-wrap">
        <div class="nf-header">
          <div>
            <div class="nf-title">NOTIFICATIONS</div>
            <div class="nf-sub">Configurable event → action → target rules</div>
          </div>
          <div class="nf-tabs">
            <button class="nf-tab active" data-tab="rules">Rules</button>
            <button class="nf-tab" data-tab="deliveries">Delivery Log</button>
          </div>
        </div>
        <div class="nf-body" id="nf-body"></div>
      </div>`;
    this.el.querySelectorAll(".nf-tab").forEach(b => {
      b.addEventListener("click", () => {
        this.tab = b.dataset.tab;
        this.el.querySelectorAll(".nf-tab").forEach(x => x.classList.toggle("active", x === b));
        this._renderBody();
      });
    });
  }

  _renderBody() {
    const body = this.el.querySelector("#nf-body");
    if (!body) return;
    if (this.tab === "deliveries") { body.innerHTML = this._deliveriesHTML(); return; }
    body.innerHTML = this._rulesHTML();
    this._wireRules(body);
  }

  _rulesHTML() {
    const rows = this.rules.map(r => this._ruleRow(r)).join("");
    return `
      <div class="nf-toolbar"><button class="nf-btn nf-btn-primary" id="nf-add">+ Add rule</button></div>
      <div class="nf-list-head">
        <span></span><span>Name</span><span>Event</span><span>Action / Target</span><span>Target</span><span></span>
      </div>
      ${rows || `<div class="nf-empty">No rules yet. Click “Add rule”.</div>`}`;
  }

  _ruleRow(r) {
    const match = r.match && r.match !== "{}" ? `<span class="nf-match">if ${esc(r.match)}</span>` : "";
    const targetEmpty = !r.target;
    const targetCls = targetEmpty ? "nf-target empty" : "nf-target";
    const targetTxt = targetEmpty ? "(unset — disabled)" : esc(r.target);
    return `
      <div class="nf-rule ${r.enabled ? "" : "disabled"}" data-id="${esc(r.id)}">
        <div class="nf-toggle ${r.enabled ? "on" : ""}" data-toggle="${esc(r.id)}" title="Enable / disable"></div>
        <div class="nf-rule-name">${esc(r.name)}${match}</div>
        <div><span class="nf-pill event">${esc(r.event)}</span></div>
        <div><span class="nf-pill action-${esc(r.action)}">${esc(r.action)}</span></div>
        <div class="${targetCls}">${targetTxt}</div>
        <div class="nf-actions">
          <button class="nf-btn nf-btn-sm" data-test="${esc(r.id)}">Test</button>
          <button class="nf-btn nf-btn-sm" data-edit="${esc(r.id)}">Edit</button>
          <button class="nf-btn nf-btn-sm nf-btn-danger" data-del="${esc(r.id)}">×</button>
        </div>
      </div>`;
  }

  _wireRules(body) {
    body.querySelector("#nf-add")?.addEventListener("click", () => this._openEditor(null));
    body.querySelectorAll("[data-toggle]").forEach(t =>
      t.addEventListener("click", () => this._toggle(t.dataset.toggle)));
    body.querySelectorAll("[data-edit]").forEach(b =>
      b.addEventListener("click", () => this._openEditor(this.rules.find(r => r.id === b.dataset.edit))));
    body.querySelectorAll("[data-del]").forEach(b =>
      b.addEventListener("click", () => this._delete(b.dataset.del)));
    body.querySelectorAll("[data-test]").forEach(b =>
      b.addEventListener("click", () => this._openEditor(this.rules.find(r => r.id === b.dataset.test), true)));
  }

  async _toggle(id) {
    const rule = this.rules.find(r => r.id === id);
    if (!rule) return;
    await fetch(`/api/notification-rules/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled: !rule.enabled }),
    });
    await this.refresh();
  }

  async _delete(id) {
    if (!confirm("Delete this rule?")) return;
    await fetch(`/api/notification-rules/${id}`, { method: "DELETE" });
    await this.refresh();
  }

  _deliveriesHTML() {
    if (!this.deliveries.length) return `<div class="nf-empty">No deliveries yet. Fire a rule to populate the log.</div>`;
    const rows = this.deliveries.map(d => {
      let line = "";
      try { line = JSON.parse(d.payload || "{}").line || ""; } catch { line = ""; }
      const code = d.status_code ? ` (${d.status_code})` : "";
      const info = d.error ? esc(d.error) : esc(line);
      return `
        <div class="nf-del">
          <span class="nf-outcome ${esc(d.outcome)}">${esc(d.outcome)}${code}</span>
          <span class="nf-pill event">${esc(d.event)}</span>
          <span>${esc(d.action)}</span>
          <span class="nf-del-line" title="${esc(d.target)}">${info}</span>
          <span style="color:#6c7086;font-size:11px;">${timeAgo(d.created_at)}</span>
        </div>`;
    }).join("");
    return `<div class="nf-del-head"><span>Outcome</span><span>Event</span><span>Action</span><span>Line / Error</span><span>When</span></div>${rows}`;
  }

  _openEditor(rule, testMode) {
    const isNew = !rule;
    rule = rule || { name: "", enabled: true, event: EVENTS[0], match: "{}", action: "message", target: "", opts: "{}" };
    const allEvents = this._allEvents();
    if (rule.event && !allEvents.includes(rule.event)) allEvents.push(rule.event);
    const eventOpts = allEvents.map(e => `<option value="${e}" ${e === rule.event ? "selected" : ""}>${e}</option>`).join("");
    const actionOpts = ACTIONS.map(a => `<option value="${a}" ${a === rule.action ? "selected" : ""}>${a}</option>`).join("");

    const overlay = document.createElement("div");
    overlay.className = "nf-modal";
    overlay.innerHTML = `
      <div class="nf-modal-box">
        <div class="nf-modal-title">${isNew ? "New rule" : (testMode ? "Test-fire: " + esc(rule.name) : "Edit rule")}</div>
        <div class="nf-field"><label>Name</label><input id="nf-f-name" value="${esc(rule.name)}" placeholder="Blocked → manager"></div>
        <div class="nf-field"><label>Event</label><select id="nf-f-event">${eventOpts}</select></div>
        <div class="nf-field"><label>Match (JSON)</label><input id="nf-f-match" value="${esc(rule.match)}" placeholder='{"assignee_is_agent":true}'>
          <div class="nf-hint">Optional conditions, e.g. {"assignee_is_agent":true}. Empty {} = match all.</div></div>
        <div class="nf-field"><label>Action</label><select id="nf-f-action">${actionOpts}</select></div>
        <div class="nf-field"><label>Target</label><input id="nf-f-target" value="${esc(rule.target)}" placeholder="agent name · role · human · https://launcher.url">
          <div class="nf-hint">Webhook/slack: URL. Message: agent name, role, "manager" (reports_to), or "human".</div></div>
        <div class="nf-field"><label>Opts (JSON)</label><input id="nf-f-opts" value="${esc(rule.opts)}" placeholder='{"priority":"P1","ttl":3600}'>
          <div class="nf-hint">ttl, priority, template, interval_hours (digest).</div></div>
        <div id="nf-test-result"></div>
        <div class="nf-modal-actions">
          <button class="nf-btn nf-btn-danger" id="nf-cancel">Cancel</button>
          <div style="display:flex;gap:8px;">
            ${isNew ? "" : `<button class="nf-btn" id="nf-testfire">Test-fire (dry)</button>`}
            ${isNew ? "" : `<button class="nf-btn" id="nf-testsend">Send for real</button>`}
            <button class="nf-btn nf-btn-primary" id="nf-save">${isNew ? "Create" : "Save"}</button>
          </div>
        </div>
      </div>`;
    document.body.appendChild(overlay);

    const close = () => overlay.remove();
    overlay.addEventListener("click", e => { if (e.target === overlay) close(); });
    overlay.querySelector("#nf-cancel").addEventListener("click", close);

    const collect = () => ({
      name: overlay.querySelector("#nf-f-name").value.trim(),
      event: overlay.querySelector("#nf-f-event").value,
      match: overlay.querySelector("#nf-f-match").value.trim() || "{}",
      action: overlay.querySelector("#nf-f-action").value,
      target: overlay.querySelector("#nf-f-target").value.trim(),
      opts: overlay.querySelector("#nf-f-opts").value.trim() || "{}",
    });

    const parseJSON = (raw, field) => {
      try { return JSON.parse(raw); } catch { alert(`Invalid JSON in ${field}`); return undefined; }
    };

    overlay.querySelector("#nf-save").addEventListener("click", async () => {
      const data = collect();
      if (!data.name) { alert("Name is required"); return; }
      const matchObj = parseJSON(data.match, "Match"); if (matchObj === undefined) return;
      const optsObj = parseJSON(data.opts, "Opts"); if (optsObj === undefined) return;
      const body = { project: this.project, name: data.name, event: data.event, match: matchObj, action: data.action, target: data.target, opts: optsObj };
      if (isNew) {
        await fetch(`/api/notification-rules`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      } else {
        await fetch(`/api/notification-rules/${rule.id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      }
      close();
      await this.refresh();
    });

    const doTest = async (send) => {
      const res = await fetch(`/api/notification-rules/${rule.id}/test-fire?send=${send}`, { method: "POST" });
      const out = overlay.querySelector("#nf-test-result");
      if (!res.ok) { out.innerHTML = `<div class="nf-testbox">Test failed (${res.status})</div>`; return; }
      const j = await res.json();
      out.innerHTML = `<div class="nf-testbox">outcome: ${esc(j.outcome)}${j.delivery?.status_code ? " (" + j.delivery.status_code + ")" : ""}${j.delivery?.error ? "\nerror: " + esc(j.delivery.error) : ""}\n\npayload:\n${esc(JSON.stringify(j.payload, null, 2))}</div>`;
      if (send) this._loadDeliveries();
    };
    overlay.querySelector("#nf-testfire")?.addEventListener("click", () => doTest(false));
    overlay.querySelector("#nf-testsend")?.addEventListener("click", () => doTest(true));
    if (testMode) doTest(false);
  }
}
