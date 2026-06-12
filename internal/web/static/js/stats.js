// stats.js — Agentic analytics panel for agent-relay
// Hand-rolled SVG charts (no chart lib). Dark theme, accessible:
// every chart ships a visually-hidden summary + a toggleable data table.

const SVGNS = "http://www.w3.org/2000/svg";

// State color/pattern map — never color-only (each state also has a label + a
// distinct SVG fill pattern for colorblind users).
const STATE_STYLE = {
  todo: { color: "#ffd93d", label: "Todo", pattern: "diag" },
  in_progress: { color: "#00e676", label: "In Progress", pattern: "solid" },
  in_review: { color: "#74b9ff", label: "In Review", pattern: "dots" },
  blocked: { color: "#ff6b6b", label: "Blocked", pattern: "cross" },
};

function esc(str) {
  if (str == null) return "";
  const d = document.createElement("div");
  d.textContent = String(str);
  return d.innerHTML;
}

// Format seconds → compact human duration.
function fmtDur(secs) {
  if (secs == null || isNaN(secs)) return "—";
  secs = Math.round(secs);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hrs = mins / 60;
  if (hrs < 24) return `${hrs.toFixed(hrs < 10 ? 1 : 0)}h`;
  const days = hrs / 24;
  return `${days.toFixed(days < 10 ? 1 : 0)}d`;
}

function fmtDate(d) {
  if (!d) return "";
  // YYYY-MM-DD → MMM D
  const [y, m, day] = d.split("-");
  const months = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
  return `${months[parseInt(m, 10) - 1]} ${parseInt(day, 10)}`;
}

const STATS_STYLES = `
.st-root {
  display: flex; flex-direction: column;
  height: 100%; width: 100%;
  background: #0a0a12; color: #e0e0e8;
  font-family: 'JetBrains Mono', monospace;
  overflow: hidden;
}
.st-toolbar {
  display: flex; align-items: center; gap: 12px;
  padding: 12px 16px; border-bottom: 1px solid #1e1e2e;
  flex-shrink: 0; flex-wrap: wrap;
}
.st-toolbar h2 { font-size: 14px; letter-spacing: 1px; margin-right: auto; color: #b6e3ff; }
.st-toolbar label { font-size: 11px; color: #9a9ab0; display: flex; align-items: center; gap: 6px; }
.st-toolbar select {
  background: #14141f; color: #e0e0e8; border: 1px solid #2a2a3e;
  border-radius: 4px; padding: 4px 8px; font-family: inherit; font-size: 11px;
}
.st-toolbar select:focus-visible { outline: 2px solid #74b9ff; outline-offset: 1px; }
.st-cycle-badge { font-size: 10px; padding: 2px 8px; border-radius: 10px; }
.st-cycle-badge.active { background: rgba(0,230,118,.18); color: #00e676; }
.st-cycle-badge.inactive { background: rgba(154,154,176,.16); color: #9a9ab0; }
.st-live-dot { width: 8px; height: 8px; border-radius: 50%; background: #00e676; box-shadow: 0 0 6px #00e676; }
.st-live-dot.stale { background: #636e72; box-shadow: none; }

.st-grid {
  flex: 1; overflow-y: auto; padding: 16px;
  display: grid; grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px; align-content: start;
}
.st-card {
  background: #11111b; border: 1px solid #1e1e2e; border-radius: 8px;
  padding: 14px; display: flex; flex-direction: column; min-width: 0;
}
.st-card.wide { grid-column: 1 / -1; }
.st-card h3 { font-size: 12px; color: #c8c8e0; margin-bottom: 4px; letter-spacing: .5px; }
.st-card .st-sub { font-size: 10px; color: #7a7a92; margin-bottom: 10px; }

.st-legend { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 8px; }
.st-legend-item { display: flex; align-items: center; gap: 5px; font-size: 10px; color: #9a9ab0; }
.st-legend-swatch { width: 11px; height: 11px; border-radius: 2px; border: 1px solid rgba(255,255,255,.15); }

.st-tiles { display: flex; flex-wrap: wrap; gap: 10px; }
.st-tile {
  background: #14141f; border: 1px solid #2a2a3e; border-radius: 6px;
  padding: 10px 12px; min-width: 120px; flex: 1;
}
.st-tile .name { font-size: 11px; color: #c8c8e0; margin-bottom: 6px; font-weight: bold; }
.st-tile .row { display: flex; justify-content: space-between; font-size: 10px; color: #9a9ab0; }
.st-tile .row b { color: #e0e0e8; }
.st-tile.idle { opacity: .6; }
.st-tile .badge { font-size: 9px; padding: 1px 6px; border-radius: 8px; }
.st-tile .badge.busy { background: rgba(0,230,118,.18); color: #00e676; }
.st-tile .badge.blocked { background: rgba(255,107,107,.18); color: #ff6b6b; }
.st-tile .badge.idle { background: rgba(154,154,176,.16); color: #9a9ab0; }

.st-toggle-table {
  align-self: flex-start; margin-top: 10px;
  background: none; border: 1px solid #2a2a3e; color: #9a9ab0;
  font-family: inherit; font-size: 10px; padding: 3px 8px; border-radius: 4px; cursor: pointer;
}
.st-toggle-table:hover { color: #e0e0e8; border-color: #444; }
.st-toggle-table:focus-visible { outline: 2px solid #74b9ff; outline-offset: 1px; }

table.st-table { width: 100%; border-collapse: collapse; margin-top: 10px; font-size: 10px; }
table.st-table th, table.st-table td { text-align: right; padding: 3px 6px; border-bottom: 1px solid #1e1e2e; }
table.st-table th:first-child, table.st-table td:first-child { text-align: left; }
table.st-table th { color: #7a7a92; font-weight: normal; }
table.st-table td { color: #c8c8e0; }

.st-empty { color: #7a7a92; font-size: 11px; padding: 24px; text-align: center; }
.st-bottleneck { color: #ff6b6b; }

.st-skel {
  background: linear-gradient(90deg, #14141f 25%, #1c1c2c 50%, #14141f 75%);
  background-size: 200% 100%; border-radius: 4px;
  animation: st-shimmer 1.4s ease-in-out infinite;
}
@keyframes st-shimmer { 0% { background-position: 200% 0; } 100% { background-position: -200% 0; } }

.st-vh {
  position: absolute !important; width: 1px; height: 1px;
  padding: 0; margin: -1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; border: 0;
}

.st-chart text { fill: #9a9ab0; font-size: 9px; }
.st-chart .grid-line { stroke: #1e1e2e; stroke-width: 1; }
.st-chart .axis-line { stroke: #2a2a3e; stroke-width: 1; }
.st-bar:hover, .st-seg:hover { opacity: .85; }
.st-point { transition: r .1s; }

@media (max-width: 900px) { .st-grid { grid-template-columns: 1fr; } .st-card.wide { grid-column: auto; } }
@media (prefers-reduced-motion: reduce) {
  .st-skel { animation: none; background: #14141f; }
  .st-bar-anim, .st-line-anim { animation: none !important; }
}
@keyframes st-grow { from { transform: scaleY(0); } to { transform: scaleY(1); } }
.st-bar-anim { transform-origin: bottom; animation: st-grow .5s ease-out; }
`;

export class StatsPanel {
  constructor(container) {
    this.container = container;
    this.project = null; // null = all projects → "default" on server
    this.cycle = "";      // "" = server default (active cycle)
    this.agent = "";      // "" = all agents
    this.data = null;
    this.loading = false;
    this._es = null;
    this._refreshTimer = null;
    this._pendingRefresh = false;
    this._lastEventAt = 0;

    this._style = document.createElement("style");
    this._style.textContent = STATS_STYLES;
    document.head.appendChild(this._style);

    this.root = document.createElement("div");
    this.root.className = "st-root";
    this.container.appendChild(this.root);

    this._buildShell();
  }

  /* ─── Public API ─── */

  setProject(project) {
    // project null/undefined → cross-project default bucket on the server.
    if (this.project === project) return;
    this.project = project;
    this.cycle = ""; // reset cycle when project context changes
  }

  show() {
    this.root.style.display = "flex";
    this._connectLive();
    this.refresh();
  }

  hide() {
    this.root.style.display = "none";
    this._disconnectLive();
  }

  destroy() {
    this._disconnectLive();
    if (this._refreshTimer) clearTimeout(this._refreshTimer);
    if (this._style.parentNode) this._style.parentNode.removeChild(this._style);
    if (this.root.parentNode) this.root.parentNode.removeChild(this.root);
  }

  /* ─── Data fetch ─── */

  _statsURL() {
    const params = new URLSearchParams();
    if (this.project) params.set("project", this.project);
    if (this.cycle) params.set("cycle", this.cycle);
    if (this.agent) params.set("agent", this.agent);
    const qs = params.toString();
    return "/api/stats" + (qs ? "?" + qs : "");
  }

  async refresh() {
    if (this.root.style.display === "none") return;
    this.loading = !this.data; // only show skeletons on first/empty load
    if (this.loading) this._renderSkeleton();
    try {
      const res = await fetch(this._statsURL());
      if (!res.ok) throw new Error("stats fetch " + res.status);
      this.data = await res.json();
      this.loading = false;
      this._render();
    } catch (err) {
      console.error("[stats] fetch error:", err);
      this.loading = false;
      this._renderError();
    }
  }

  /* ─── Live updates (SSE) ─── */

  _connectLive() {
    if (this._es) return;
    try {
      this._es = new EventSource("/api/events/stream");
      this._es.onmessage = (e) => {
        let evt;
        try { evt = JSON.parse(e.data); } catch { return; }
        if (!evt || typeof evt.type !== "string") return;
        if (!evt.type.startsWith("task.")) return;
        // Project scope: if we're scoped to a project, ignore others.
        if (this.project && evt.project && evt.project !== this.project) return;
        this._onTaskEvent(evt);
      };
      this._es.onerror = () => { this._setLive(false); };
      this._es.onopen = () => { this._setLive(true); };
    } catch (err) {
      console.warn("[stats] SSE unavailable:", err);
    }
  }

  _disconnectLive() {
    if (this._es) { this._es.close(); this._es = null; }
  }

  _onTaskEvent(evt) {
    this._lastEventAt = Date.now();
    this._setLive(true);
    // Update load tiles immediately from a quick optimistic re-fetch of load only
    // would need a separate endpoint; instead debounce a full re-fetch (>=5s) and
    // do a lightweight local nudge on the live indicator now.
    this._scheduleRefresh();
  }

  // Debounced full refresh, >=5s between fetches (PRD FR-4).
  _scheduleRefresh() {
    if (this._refreshTimer) { this._pendingRefresh = true; return; }
    this._refreshTimer = setTimeout(() => {
      this._refreshTimer = null;
      this.refresh();
      if (this._pendingRefresh) {
        this._pendingRefresh = false;
        this._scheduleRefresh();
      }
    }, 5000);
  }

  _setLive(on) {
    if (!this._liveDot) return;
    this._liveDot.classList.toggle("stale", !on);
    this._liveDot.title = on ? "Live — updates as agents work" : "Reconnecting…";
  }

  /* ─── Shell ─── */

  _buildShell() {
    const bar = document.createElement("div");
    bar.className = "st-toolbar";
    bar.innerHTML = `
      <h2>AGENT ANALYTICS</h2>
      <span class="st-live-dot" title="Live"></span>
      <label>Cycle
        <select class="st-cycle-select" aria-label="Cycle filter"></select>
      </label>
      <label>Agent
        <select class="st-agent-select" aria-label="Agent filter">
          <option value="">All agents</option>
        </select>
      </label>
      <span class="st-cycle-badge inactive">—</span>`;
    this.root.appendChild(bar);

    this._liveDot = bar.querySelector(".st-live-dot");
    this._cycleSelect = bar.querySelector(".st-cycle-select");
    this._agentSelect = bar.querySelector(".st-agent-select");
    this._cycleBadge = bar.querySelector(".st-cycle-badge");

    this._cycleSelect.addEventListener("change", () => {
      this.cycle = this._cycleSelect.value;
      this.data = null;
      this.refresh();
    });
    this._agentSelect.addEventListener("change", () => {
      this.agent = this._agentSelect.value;
      this.refresh();
    });

    this.grid = document.createElement("div");
    this.grid.className = "st-grid";
    this.root.appendChild(this.grid);
  }

  _syncSelectors() {
    const d = this.data;
    if (!d) return;
    // Cycle options.
    const cycles = d.cycles || [];
    const opts = ['<option value="all">All time</option>'];
    for (const c of cycles) {
      opts.push(`<option value="${esc(c.id)}">${esc(c.name)}${c.active ? " ●" : ""}</option>`);
    }
    this._cycleSelect.innerHTML = opts.join("");
    // Selected value: reflect server-resolved scope when user hasn't chosen.
    const sel = this.cycle || (d.cycle && d.cycle.id) || "all";
    this._cycleSelect.value = sel;
    if (this._cycleSelect.value !== sel) this._cycleSelect.value = "all";

    // Agent options.
    const agents = d.agents || [];
    const aopts = ['<option value="">All agents</option>'];
    for (const a of agents) aopts.push(`<option value="${esc(a)}">${esc(a)}</option>`);
    this._agentSelect.innerHTML = aopts.join("");
    this._agentSelect.value = this.agent;

    // Cycle badge.
    const sc = d.cycle || {};
    this._cycleBadge.textContent = sc.active ? "ACTIVE CYCLE" : (sc.id === "all" ? "ALL TIME" : "PAST CYCLE");
    this._cycleBadge.className = "st-cycle-badge " + (sc.active ? "active" : "inactive");
  }

  /* ─── Render states ─── */

  _renderSkeleton() {
    this.grid.innerHTML = "";
    for (let i = 0; i < 6; i++) {
      const card = document.createElement("div");
      card.className = "st-card" + (i >= 4 ? " wide" : "");
      card.innerHTML = `<div class="st-skel" style="height:14px;width:40%;margin-bottom:12px"></div>
        <div class="st-skel" style="height:120px;width:100%"></div>`;
      this.grid.appendChild(card);
    }
  }

  _renderError() {
    this.grid.innerHTML = `<div class="st-card wide"><div class="st-empty">Failed to load stats. Retrying on next update…</div></div>`;
  }

  _render() {
    this._syncSelectors();
    this.grid.innerHTML = "";

    const d = this.data;
    if (!d || d.empty) {
      const msg = (d && d.cycle && d.cycle.id === "all")
        ? "No data yet — no completed or in-flight tasks."
        : "No data yet — this cycle hasn't started, or has no tasks.";
      const note = (d && (!d.cycles || d.cycles.length === 0))
        ? " (No cycles detected — native mode shows all-time.)" : "";
      this.grid.innerHTML = `<div class="st-card wide"><div class="st-empty">${esc(msg)}${esc(note)}</div></div>`;
      return;
    }

    this.grid.appendChild(this._cardCycleTime(d.cycle_time));
    this.grid.appendChild(this._cardTimeInState(d.time_in_state));
    this.grid.appendChild(this._cardBlocked(d.blocked));
    this.grid.appendChild(this._cardLoad(d.load));
    this.grid.appendChild(this._cardThroughput(d.throughput));
    this.grid.appendChild(this._cardBurndown(d.burndown));
  }

  /* ─── Helpers for cards ─── */

  _card(title, sub) {
    const c = document.createElement("div");
    c.className = "st-card";
    const h = document.createElement("h3");
    h.textContent = title;
    c.appendChild(h);
    if (sub) {
      const s = document.createElement("div");
      s.className = "st-sub";
      s.textContent = sub;
      c.appendChild(s);
    }
    return c;
  }

  // Attach an accessible summary + toggleable data table to a card.
  _attachTable(card, summary, headers, rows) {
    const vh = document.createElement("p");
    vh.className = "st-vh";
    vh.textContent = summary;
    card.appendChild(vh);

    const btn = document.createElement("button");
    btn.className = "st-toggle-table";
    btn.type = "button";
    btn.setAttribute("aria-expanded", "false");
    btn.textContent = "Show table";

    const tbl = document.createElement("table");
    tbl.className = "st-table";
    tbl.style.display = "none";
    const thead = `<thead><tr>${headers.map(h => `<th>${esc(h)}</th>`).join("")}</tr></thead>`;
    const tbody = `<tbody>${rows.map(r => `<tr>${r.map(c => `<td>${esc(c)}</td>`).join("")}</tr>`).join("")}</tbody>`;
    tbl.innerHTML = thead + tbody;

    btn.addEventListener("click", () => {
      const open = tbl.style.display !== "none";
      tbl.style.display = open ? "none" : "table";
      btn.setAttribute("aria-expanded", String(!open));
      btn.textContent = open ? "Show table" : "Hide table";
    });

    card.appendChild(btn);
    card.appendChild(tbl);
  }

  _legend(items) {
    const l = document.createElement("div");
    l.className = "st-legend";
    for (const it of items) {
      const item = document.createElement("span");
      item.className = "st-legend-item";
      item.innerHTML = `<span class="st-legend-swatch" style="background:${it.color}"></span>${esc(it.label)}`;
      l.appendChild(item);
    }
    return l;
  }

  /* ─── Card: per-agent cycle time (grouped bar) ─── */

  _cardCycleTime(ct) {
    const card = this._card("Agent Cycle Time", "median claim→review / claim→done");
    const per = (ct && ct.per_agent) || [];
    if (per.length === 0) {
      card.appendChild(this._emptyNote("No completed work to measure yet."));
      return card;
    }
    const series = [
      { key: "claim_to_review", color: STATE_STYLE.in_review.color, label: "→ Review (median)" },
      { key: "claim_to_done", color: STATE_STYLE.in_progress.color, label: "→ Done (median)" },
    ];
    const maxV = Math.max(1, ...per.flatMap(a => series.map(s => a[s.key].median)));
    card.appendChild(this._groupedBar(per, series, maxV, a => a.agent, (a, s) => a[s.key].median, fmtDur));
    card.appendChild(this._legend(series.map(s => ({ color: s.color, label: s.label }))));

    const rows = per.map(a => [
      a.agent,
      fmtDur(a.claim_to_review.median), fmtDur(a.claim_to_review.p90),
      fmtDur(a.claim_to_done.median), fmtDur(a.claim_to_done.p90),
      a.claim_to_done.count,
    ]);
    const ov = ct.overall;
    const summary = `Agent cycle time. Overall median claim to review ${fmtDur(ov.claim_to_review.median)}, p90 ${fmtDur(ov.claim_to_review.p90)}; claim to done median ${fmtDur(ov.claim_to_done.median)}, p90 ${fmtDur(ov.claim_to_done.p90)}, over ${ov.claim_to_done.count} tasks.`;
    this._attachTable(card, summary,
      ["Agent", "→Review med", "→Review p90", "→Done med", "→Done p90", "n"], rows);
    return card;
  }

  /* ─── Card: time in state (stacked bar) ─── */

  _cardTimeInState(tis) {
    const card = this._card("Time in State", "median duration per lifecycle state");
    if (!tis || (tis.todo.count + tis.in_progress.count + tis.in_review.count) === 0) {
      card.appendChild(this._emptyNote("No state transitions recorded yet."));
      return card;
    }
    const states = [
      { key: "todo", ...STATE_STYLE.todo },
      { key: "in_progress", ...STATE_STYLE.in_progress },
      { key: "in_review", ...STATE_STYLE.in_review },
    ];
    const vals = states.map(s => ({ ...s, v: tis[s.key].median }));
    card.appendChild(this._stackedSingleBar(vals, fmtDur));

    const legendItems = states.map(s => {
      const isBn = tis.bottleneck === s.key;
      return { color: s.color, label: s.label + (isBn ? " ⚠ bottleneck" : "") };
    });
    card.appendChild(this._legend(legendItems));

    if (tis.bottleneck) {
      const bn = document.createElement("div");
      bn.className = "st-sub st-bottleneck";
      bn.style.marginTop = "8px";
      bn.textContent = `Bottleneck: ${STATE_STYLE[tis.bottleneck] ? STATE_STYLE[tis.bottleneck].label : tis.bottleneck}`;
      card.appendChild(bn);
    }

    const rows = states.map(s => [s.label, fmtDur(tis[s.key].median), fmtDur(tis[s.key].avg), tis[s.key].count]);
    const summary = `Time in state medians: Todo ${fmtDur(tis.todo.median)}, In Progress ${fmtDur(tis.in_progress.median)}, In Review ${fmtDur(tis.in_review.median)}. Bottleneck state: ${tis.bottleneck || "none"}.`;
    this._attachTable(card, summary, ["State", "Median", "Avg", "n"], rows);
    return card;
  }

  /* ─── Card: blocked breakdown ─── */

  _cardBlocked(b) {
    const card = this._card("Blocked Time", "total + per-agent, with current blockers");
    if (!b || b.episode_count === 0) {
      card.appendChild(this._emptyNote("No blocked episodes — nothing stuck."));
      return card;
    }
    const summaryLine = document.createElement("div");
    summaryLine.className = "st-sub";
    summaryLine.innerHTML = `Total <b style="color:#ff6b6b">${esc(fmtDur(b.total_seconds))}</b> across ${b.episode_count} episode${b.episode_count === 1 ? "" : "s"}.`;
    card.appendChild(summaryLine);

    const per = b.per_agent || [];
    if (per.length) {
      const maxV = Math.max(1, ...per.map(a => a.total_seconds));
      const series = [{ key: "total_seconds", color: STATE_STYLE.blocked.color, label: "Total blocked" }];
      card.appendChild(this._groupedBar(per, series, maxV, a => a.agent, (a, s) => a[s.key], fmtDur));
    }

    if (b.currently_blocked && b.currently_blocked.length) {
      const cb = document.createElement("div");
      cb.className = "st-sub";
      cb.style.marginTop = "10px";
      cb.innerHTML = `<b style="color:#ff6b6b">Currently blocked:</b>`;
      card.appendChild(cb);
      const list = document.createElement("div");
      list.className = "st-tiles";
      for (const t of b.currently_blocked) {
        const tile = document.createElement("div");
        tile.className = "st-tile blocked";
        tile.innerHTML = `<div class="name">${esc(t.title || t.task_id)}</div>
          <div class="row"><span>${esc(t.agent || "—")}${t.linear_key ? " · " + esc(t.linear_key) : ""}</span><b>${esc(fmtDur(t.since_seconds))}</b></div>`;
        list.appendChild(tile);
      }
      card.appendChild(list);
    }

    const rows = per.map(a => [a.agent, fmtDur(a.total_seconds), fmtDur(a.avg_seconds), a.episode_count]);
    const summary = `Blocked time total ${fmtDur(b.total_seconds)} across ${b.episode_count} episodes. ${(b.currently_blocked || []).length} task(s) currently blocked.`;
    this._attachTable(card, summary, ["Agent", "Total", "Avg", "Episodes"], rows);
    return card;
  }

  /* ─── Card: current load tiles ─── */

  _cardLoad(load) {
    const card = this._card("Load Now", "claimed / blocked / idle per agent");
    load = load || [];
    if (load.length === 0) {
      card.appendChild(this._emptyNote("No active agents right now."));
      return card;
    }
    const tiles = document.createElement("div");
    tiles.className = "st-tiles";
    for (const l of load) {
      const tile = document.createElement("div");
      tile.className = "st-tile" + (l.idle ? " idle" : "");
      let badge = `<span class="badge idle">idle</span>`;
      if (l.blocked > 0) badge = `<span class="badge blocked">blocked</span>`;
      else if (l.claimed > 0) badge = `<span class="badge busy">active</span>`;
      tile.innerHTML = `<div class="name">${esc(l.agent)} ${badge}</div>
        <div class="row"><span>Claimed</span><b>${l.claimed}</b></div>
        <div class="row"><span>Blocked</span><b>${l.blocked}</b></div>`;
      tiles.appendChild(tile);
    }
    card.appendChild(tiles);
    const rows = load.map(l => [l.agent, l.claimed, l.blocked, l.idle ? "idle" : "active"]);
    const busy = load.filter(l => !l.idle).length;
    this._attachTable(card, `Current load: ${busy} of ${load.length} agents active.`,
      ["Agent", "Claimed", "Blocked", "Status"], rows);
    return card;
  }

  /* ─── Card: throughput over cycle (cumulative line) ─── */

  _cardThroughput(tp) {
    const card = this._card("Throughput", "cumulative tasks & points completed");
    card.classList.add("wide");
    const series = (tp && tp.series) || [];
    const head = document.createElement("div");
    head.className = "st-sub";
    head.innerHTML = `Done: <b>${tp ? tp.tasks_done : 0}</b> tasks · <b>${tp ? tp.points_done : 0}</b> points`;
    card.appendChild(head);

    if (series.length === 0) {
      card.appendChild(this._emptyNote("No completions yet — the curve fills as agents finish work."));
      return card;
    }
    const lines = [
      { key: "cumulative_points", color: STATE_STYLE.in_progress.color, label: "Points" },
      { key: "cumulative_tasks", color: STATE_STYLE.in_review.color, label: "Tasks" },
    ];
    card.appendChild(this._lineChart(series, lines, d => d.date));
    card.appendChild(this._legend(lines.map(l => ({ color: l.color, label: l.label }))));

    const rows = series.map(s => [fmtDate(s.date), s.cumulative_tasks, s.cumulative_points]);
    const last = series[series.length - 1];
    this._attachTable(card, `Throughput cumulative. By ${fmtDate(last.date)}: ${last.cumulative_tasks} tasks, ${last.cumulative_points} points completed.`,
      ["Day", "Cumulative tasks", "Cumulative points"], rows);
    return card;
  }

  /* ─── Card: burndown (line) ─── */

  _cardBurndown(bd) {
    const card = this._card("Burndown", "remaining points vs ideal");
    card.classList.add("wide");
    const series = (bd && bd.series) || [];
    const head = document.createElement("div");
    head.className = "st-sub";
    head.innerHTML = `Scope: <b>${bd ? bd.total_points : 0}</b> points / <b>${bd ? bd.total_tasks : 0}</b> tasks · Remaining: <b>${bd ? bd.remaining_points : 0}</b> points`;
    card.appendChild(head);

    if (series.length === 0) {
      card.appendChild(this._emptyNote("No burndown yet — cycle hasn't started or has no scope."));
      return card;
    }
    const lines = [
      { key: "remaining_points", color: STATE_STYLE.blocked.color, label: "Remaining" },
      { key: "ideal", color: "#636e72", label: "Ideal", dashed: true },
    ];
    card.appendChild(this._lineChart(series, lines, d => d.date));
    card.appendChild(this._legend(lines.map(l => ({ color: l.color, label: l.label }))));

    const rows = series.map(s => [fmtDate(s.date), s.remaining_points, s.remaining_tasks, s.ideal]);
    this._attachTable(card, `Burndown. Scope ${bd.total_points} points, ${bd.remaining_points} remaining.`,
      ["Day", "Remaining points", "Remaining tasks", "Ideal"], rows);
    return card;
  }

  _emptyNote(text) {
    const e = document.createElement("div");
    e.className = "st-empty";
    e.textContent = text;
    return e;
  }

  /* ─── SVG chart primitives ─── */

  _svg(w, h) {
    const s = document.createElementNS(SVGNS, "svg");
    s.setAttribute("viewBox", `0 0 ${w} ${h}`);
    s.setAttribute("class", "st-chart");
    s.setAttribute("width", "100%");
    s.setAttribute("preserveAspectRatio", "xMidYMid meet");
    s.setAttribute("role", "img");
    s.style.maxHeight = h + "px";
    return s;
  }

  _line(svg, x1, y1, x2, y2, cls) {
    const l = document.createElementNS(SVGNS, "line");
    l.setAttribute("x1", x1); l.setAttribute("y1", y1);
    l.setAttribute("x2", x2); l.setAttribute("y2", y2);
    if (cls) l.setAttribute("class", cls);
    svg.appendChild(l);
  }

  _text(svg, x, y, str, anchor) {
    const t = document.createElementNS(SVGNS, "text");
    t.setAttribute("x", x); t.setAttribute("y", y);
    if (anchor) t.setAttribute("text-anchor", anchor);
    t.textContent = str;
    svg.appendChild(t);
  }

  // Grouped vertical bar chart: groups (agents) × series.
  _groupedBar(items, series, maxV, labelFn, valFn, fmt) {
    const W = 520, H = 180, padL = 36, padB = 34, padT = 10, padR = 10;
    const svg = this._svg(W, H);
    const plotW = W - padL - padR, plotH = H - padT - padB;
    // gridlines
    for (let g = 0; g <= 2; g++) {
      const y = padT + (plotH * g) / 2;
      this._line(svg, padL, y, W - padR, y, "grid-line");
      this._text(svg, padL - 4, y + 3, fmt(maxV * (1 - g / 2)), "end");
    }
    this._line(svg, padL, padT, padL, padT + plotH, "axis-line");

    const groupW = plotW / items.length;
    const barW = Math.min(28, (groupW - 8) / series.length);
    items.forEach((item, i) => {
      const gx = padL + groupW * i + (groupW - barW * series.length) / 2;
      series.forEach((s, si) => {
        const v = valFn(item, s) || 0;
        const bh = maxV > 0 ? (v / maxV) * plotH : 0;
        const x = gx + si * barW;
        const y = padT + plotH - bh;
        const r = document.createElementNS(SVGNS, "rect");
        r.setAttribute("x", x); r.setAttribute("y", y);
        r.setAttribute("width", Math.max(2, barW - 2)); r.setAttribute("height", Math.max(0, bh));
        r.setAttribute("fill", s.color);
        r.setAttribute("class", "st-bar st-bar-anim");
        const title = document.createElementNS(SVGNS, "title");
        title.textContent = `${labelFn(item)} · ${s.label}: ${fmt(v)}`;
        r.appendChild(title);
        svg.appendChild(r);
      });
      this._text(svg, padL + groupW * i + groupW / 2, H - padB + 14, this._trunc(labelFn(item), 8), "middle");
    });
    const wrap = document.createElement("div");
    wrap.appendChild(svg);
    return wrap;
  }

  // Single horizontal stacked bar (time-in-state distribution).
  _stackedSingleBar(segments, fmt) {
    const W = 520, H = 70, padL = 4, padR = 4, padT = 16, barH = 28;
    const svg = this._svg(W, H);
    const total = segments.reduce((a, s) => a + (s.v || 0), 0) || 1;
    let x = padL;
    const plotW = W - padL - padR;
    for (const s of segments) {
      const w = ((s.v || 0) / total) * plotW;
      const r = document.createElementNS(SVGNS, "rect");
      r.setAttribute("x", x); r.setAttribute("y", padT);
      r.setAttribute("width", Math.max(0, w)); r.setAttribute("height", barH);
      r.setAttribute("fill", s.color);
      r.setAttribute("class", "st-seg");
      const title = document.createElementNS(SVGNS, "title");
      title.textContent = `${s.label}: ${fmt(s.v)}`;
      r.appendChild(title);
      svg.appendChild(r);
      if (w > 40) this._text(svg, x + w / 2, padT + barH / 2 + 3, fmt(s.v), "middle");
      x += w;
    }
    const wrap = document.createElement("div");
    wrap.appendChild(svg);
    return wrap;
  }

  // Multi-series line chart over an ordered series of buckets.
  _lineChart(data, lines, xLabelFn) {
    const W = 1040, H = 200, padL = 40, padR = 12, padT = 12, padB = 28;
    const svg = this._svg(W, H);
    const plotW = W - padL - padR, plotH = H - padT - padB;
    const n = data.length;
    let maxV = 1;
    for (const d of data) for (const l of lines) maxV = Math.max(maxV, d[l.key] || 0);

    for (let g = 0; g <= 4; g++) {
      const y = padT + (plotH * g) / 4;
      this._line(svg, padL, y, W - padR, y, "grid-line");
      this._text(svg, padL - 4, y + 3, Math.round(maxV * (1 - g / 4)), "end");
    }
    this._line(svg, padL, padT, padL, padT + plotH, "axis-line");
    this._line(svg, padL, padT + plotH, W - padR, padT + plotH, "axis-line");

    const xAt = (i) => n <= 1 ? padL + plotW / 2 : padL + (plotW * i) / (n - 1);
    const yAt = (v) => padT + plotH - (maxV > 0 ? ((v || 0) / maxV) * plotH : 0);

    for (const l of lines) {
      const pts = data.map((d, i) => `${xAt(i).toFixed(1)},${yAt(d[l.key]).toFixed(1)}`).join(" ");
      const poly = document.createElementNS(SVGNS, "polyline");
      poly.setAttribute("points", pts);
      poly.setAttribute("fill", "none");
      poly.setAttribute("stroke", l.color);
      poly.setAttribute("stroke-width", "2");
      if (l.dashed) poly.setAttribute("stroke-dasharray", "5 4");
      poly.setAttribute("class", "st-line-anim");
      svg.appendChild(poly);
      // points + tooltips
      data.forEach((d, i) => {
        const c = document.createElementNS(SVGNS, "circle");
        c.setAttribute("cx", xAt(i)); c.setAttribute("cy", yAt(d[l.key]));
        c.setAttribute("r", n > 30 ? 1.5 : 2.5);
        c.setAttribute("fill", l.color);
        c.setAttribute("class", "st-point");
        const title = document.createElementNS(SVGNS, "title");
        title.textContent = `${xLabelFn(d)} · ${l.label}: ${d[l.key]}`;
        c.appendChild(title);
        svg.appendChild(c);
      });
    }
    // x labels (first, mid, last to avoid crowding)
    const idxs = n <= 1 ? [0] : [0, Math.floor((n - 1) / 2), n - 1];
    for (const i of new Set(idxs)) {
      this._text(svg, xAt(i), H - 8, fmtDate(xLabelFn(data[i])), "middle");
    }
    const wrap = document.createElement("div");
    wrap.appendChild(svg);
    return wrap;
  }

  _trunc(s, n) {
    s = String(s || "");
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }
}
