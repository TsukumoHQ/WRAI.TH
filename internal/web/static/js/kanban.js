// kanban.js — agentic task board (read-replica of the Linear mirror + relay overlay)
//
// Renders the local `tasks` table only — one fetch per cycle, zero Linear
// round-trips. Columns: Backlog (collapsed) / Todo / In Progress / In Review /
// Done. State maps from linear_state when source=linear, else from native
// status. Cards carry the execution overlay (claimed-by agent color, blocked
// badge, "in review N min"), child roll-up, and a read-only detail panel.
//
// Mode-aware: linear_mode → read-only (cards link out to external_url, no create
// form); native → writable .kb-form create/edit. SSE drives in-place upserts
// with animated column moves (respecting prefers-reduced-motion).

import { PALETTE_COLORS } from "./sprite.js";

// ── Columns ──────────────────────────────────────────────────────────────
// The five board columns in render order. Backlog is collapsed by default.
const COLUMNS = ["backlog", "todo", "in-progress", "in-review", "done"];

const COLUMN_LABELS = {
  backlog: "BACKLOG",
  todo: "TODO",
  "in-progress": "IN PROGRESS",
  "in-review": "IN REVIEW",
  done: "DONE",
};

const COLUMN_COLORS = {
  backlog: "#636e72",
  todo: "#ffd93d",
  "in-progress": "#00e676",
  "in-review": "#74b9ff",
  done: "#6c5ce7",
};

const PRIORITY_COLORS = {
  P0: "#ff6b6b",
  P1: "#ffa502",
  P2: "#a29bfe",
  P3: "#636e72",
};

// Native status → board column. `blocked` keeps its underlying column (we badge
// it rather than giving it a lane), so it falls through to whatever it was.
const NATIVE_STATUS_COLUMN = {
  pending: "todo",
  accepted: "in-progress",
  "in-progress": "in-progress",
  "in-review": "in-review",
  done: "done",
  cancelled: "done",
};

// Linear workflow state (by lowercased name) → board column. Linear uses state
// *types* (backlog/unstarted/started/completed) plus free-text names; we match
// common names and fall back to the type-ish keywords.
function linearStateColumn(state) {
  if (!state) return "todo";
  const s = String(state).toLowerCase();
  if (s.includes("backlog")) return "backlog";
  if (s.includes("review")) return "in-review";
  if (s.includes("progress") || s.includes("started") || s.includes("doing")) return "in-progress";
  if (s.includes("done") || s.includes("complete") || s.includes("merged") || s.includes("closed") || s.includes("cancel")) return "done";
  if (s.includes("todo") || s.includes("to do") || s.includes("unstarted") || s.includes("ready") || s.includes("triage")) return "todo";
  return "todo";
}

// columnFor maps a task to its board column, source-aware.
function columnFor(task) {
  if (task.source === "linear" && task.linear_state) {
    return linearStateColumn(task.linear_state);
  }
  return NATIVE_STATUS_COLUMN[task.status] || "todo";
}

// ── Agent color (reused from the canvas sprite system) ─────────────────────
// Same hash the sprite generator uses, so a claimed card "belongs" to its agent
// with the identical neon accent shown on the canvas.
function hashName(name) {
  let hash = 0;
  for (let i = 0; i < name.length; i++) {
    hash = ((hash << 5) - hash + name.charCodeAt(i)) | 0;
  }
  return Math.abs(hash);
}

function agentColor(name) {
  if (!name) return "#a29bfe";
  return PALETTE_COLORS[hashName(name) % PALETTE_COLORS.length];
}

// ── Time helpers ──────────────────────────────────────────────────────────
function timeAgo(dateStr) {
  if (!dateStr) return "";
  const diff = Date.now() - new Date(dateStr).getTime();
  const secs = Math.floor(diff / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

function minsSince(dateStr) {
  if (!dateStr) return 0;
  return Math.max(0, Math.floor((Date.now() - new Date(dateStr).getTime()) / 60000));
}

function fmtTS(ts) {
  if (!ts) return "—";
  return new Date(ts).toLocaleString(undefined, {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  });
}

function esc(str) {
  if (str === undefined || str === null) return "";
  const d = document.createElement("div");
  d.textContent = String(str);
  return d.innerHTML;
}

function parseLabels(raw) {
  if (!raw) return [];
  if (Array.isArray(raw)) return raw;
  try {
    const v = JSON.parse(raw);
    return Array.isArray(v) ? v : [];
  } catch {
    return [];
  }
}

function parseBlockedPeriods(raw) {
  if (!raw) return [];
  if (Array.isArray(raw)) return raw;
  try {
    const v = JSON.parse(raw);
    return Array.isArray(v) ? v : [];
  } catch {
    return [];
  }
}

function isBlocked(task) {
  if (task.status === "blocked") return true;
  // An open blocked window (no end) means currently blocked even if linear_state moved on.
  return parseBlockedPeriods(task.blocked_periods).some((p) => p && p.start && !p.end);
}

// PR/branch link derived from linear_key (SYN-123 → conventional branch name).
function branchHint(task) {
  if (!task.linear_key) return null;
  return task.linear_key.toLowerCase();
}

const REDUCE_MOTION = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

const KANBAN_STYLES = `
@keyframes kb-slideIn { from { opacity: 0; transform: translateY(8px); } to { opacity: 1; transform: translateY(0); } }
@keyframes kb-formIn { from { opacity: 0; transform: scale(0.95); } to { opacity: 1; transform: scale(1); } }
@keyframes kb-p0pulse {
  0%, 100% { box-shadow: 0 0 6px rgba(255,107,107,0.4); border-color: rgba(255,107,107,0.6); }
  50%      { box-shadow: 0 0 14px rgba(255,107,107,0.8); border-color: rgba(255,107,107,1); }
}
@keyframes kb-moved {
  0%   { box-shadow: 0 0 0 0 rgba(108,92,231,0.0); }
  30%  { box-shadow: 0 0 18px 2px rgba(108,92,231,0.55); border-color: rgba(108,92,231,0.9); }
  100% { box-shadow: 0 0 0 0 rgba(108,92,231,0.0); }
}
@keyframes kb-flash {
  0%, 100% { box-shadow: 0 0 16px rgba(108,92,231,0.3); }
  50% { box-shadow: 0 0 24px rgba(108,92,231,0.6); border-color: rgba(108,92,231,1); }
}

.kb-root {
  font-family: 'JetBrains Mono', monospace;
  background: #0a0a12; color: #dfe6e9;
  width: 100%; height: 100%;
  display: flex; flex-direction: column;
  overflow: hidden; position: relative;
}

/* Header */
.kb-header {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 20px 10px; gap: 16px;
  border-bottom: 1px solid rgba(108,92,231,0.2); flex-shrink: 0;
}
.kb-header-left { display: flex; align-items: baseline; gap: 14px; min-width: 0; }
.kb-header h2 {
  margin: 0; font-size: 15px; font-weight: 700; letter-spacing: 2px;
  text-transform: uppercase; color: #6c5ce7; text-shadow: 0 0 10px rgba(108,92,231,0.5);
}
.kb-cycle-info { font-size: 10px; color: #8d8da3; letter-spacing: 0.5px; white-space: nowrap; }
.kb-cycle-info b { color: #a29bfe; font-weight: 600; }
.kb-mode-badge {
  font-size: 8px; font-weight: 700; letter-spacing: 1px; text-transform: uppercase;
  padding: 2px 6px; border-radius: 2px; border: 1px solid;
}
.kb-mode-badge--linear { color: #74b9ff; border-color: rgba(116,185,255,0.4); background: rgba(116,185,255,0.08); }
.kb-mode-badge--native { color: #00e676; border-color: rgba(0,230,118,0.35); background: rgba(0,230,118,0.06); }
.kb-header-right { display: flex; align-items: center; gap: 10px; }
.kb-cycle-select {
  background: rgba(30,30,50,0.6); border: 1px solid rgba(108,92,231,0.25);
  color: #dfe6e9; font-family: 'JetBrains Mono', monospace; font-size: 10px;
  padding: 4px 8px; border-radius: 2px; outline: none; cursor: pointer;
}
.kb-cycle-select:focus { border-color: rgba(108,92,231,0.6); }
.kb-add-btn {
  width: 30px; height: 30px; background: rgba(108,92,231,0.15);
  border: 1px solid rgba(108,92,231,0.35); color: #6c5ce7;
  font-size: 18px; font-family: 'JetBrains Mono', monospace; cursor: pointer;
  display: flex; align-items: center; justify-content: center;
  transition: all 0.2s; line-height: 1;
}
.kb-add-btn:hover { background: rgba(108,92,231,0.3); box-shadow: 0 0 12px rgba(108,92,231,0.4); }
.kb-add-btn:focus-visible { outline: 2px solid #a29bfe; outline-offset: 2px; }

/* Board */
.kb-board { display: flex; gap: 12px; padding: 14px 16px; flex: 1; overflow-x: auto; overflow-y: hidden; }

/* Column */
.kb-col {
  flex: 1; min-width: 220px; max-width: 360px;
  display: flex; flex-direction: column;
  background: rgba(15,15,26,0.95); border: 1px solid rgba(108,92,231,0.15);
  border-radius: 4px; overflow: hidden;
}
.kb-col--collapsed { flex: 0 0 44px; min-width: 44px; max-width: 44px; }
.kb-col-header {
  padding: 10px 12px 8px; text-transform: uppercase; font-size: 11px; font-weight: 700;
  letter-spacing: 2px; display: flex; align-items: center; gap: 8px;
  border-bottom: 2px solid rgba(108,92,231,0.25); flex-shrink: 0; cursor: pointer; user-select: none;
}
.kb-col--collapsed .kb-col-header {
  writing-mode: vertical-rl; transform: rotate(180deg);
  border-bottom: none; height: 100%; justify-content: flex-start; padding: 12px 12px;
}
.kb-col-count {
  font-size: 10px; background: rgba(108,92,231,0.15); color: #a29bfe;
  padding: 1px 6px; border-radius: 2px;
}
.kb-col--collapsed .kb-col-body { display: none; }
.kb-col-body { flex: 1; overflow-y: auto; padding: 8px; display: flex; flex-direction: column; gap: 8px; }
.kb-col-body::-webkit-scrollbar { width: 4px; }
.kb-col-body::-webkit-scrollbar-thumb { background: rgba(108,92,231,0.3); border-radius: 2px; }
.kb-col-empty {
  flex: 1; display: flex; align-items: center; justify-content: center;
  color: #444; font-size: 10px; letter-spacing: 1px; text-transform: uppercase; padding: 20px 0;
}

/* Card */
.kb-card {
  background: rgba(30,30,50,0.6); border: 1px solid rgba(108,92,231,0.15);
  border-radius: 3px; padding: 10px; cursor: pointer; transition: all 0.15s; position: relative;
  animation: kb-slideIn 0.25s ease-out;
}
.kb-card:hover { border-color: rgba(108,92,231,0.4); box-shadow: 0 0 10px rgba(108,92,231,0.15); transform: translateY(-1px); }
.kb-card:focus-visible { outline: 2px solid #a29bfe; outline-offset: 2px; }
.kb-card.kb-p0 { border-color: rgba(255,107,107,0.6); animation: kb-p0pulse 2s ease-in-out infinite, kb-slideIn 0.25s ease-out; }
.kb-card.kb-blocked { border-left: 3px solid #ff6b6b; background: rgba(40,16,16,0.6); }
.kb-card.kb-moved { animation: kb-moved 1.1s ease-out; }
.kb-card.kb-highlight { animation: kb-flash 0.6s ease-in-out 3; border-color: rgba(108,92,231,0.8); }
.kb-card[data-claimed] { border-left: 3px solid var(--kb-agent-color, #a29bfe); }

.kb-card-top { display: flex; align-items: center; gap: 6px; margin-bottom: 6px; flex-wrap: wrap; }
.kb-key {
  font-size: 9px; font-weight: 700; letter-spacing: 0.5px; padding: 1px 5px; border-radius: 2px;
  color: #74b9ff; background: rgba(116,185,255,0.12); border: 1px solid rgba(116,185,255,0.3);
}
.kb-badge { font-size: 9px; font-weight: 700; padding: 1px 5px; border-radius: 2px; letter-spacing: 1px; }
.kb-points {
  font-size: 9px; font-weight: 700; color: #ffd93d; background: rgba(255,217,61,0.12);
  border: 1px solid rgba(255,217,61,0.3); padding: 1px 5px; border-radius: 2px;
}
.kb-time { font-size: 9px; color: #636e72; margin-left: auto; }
.kb-card-title { font-size: 12px; font-weight: 600; color: #dfe6e9; margin-bottom: 6px; line-height: 1.35; word-break: break-word; }

.kb-card-meta { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; font-size: 10px; }
.kb-label {
  font-size: 8px; padding: 0 5px; border-radius: 8px; color: #b2bec3;
  background: rgba(255,255,255,0.06); border: 1px solid rgba(255,255,255,0.1); line-height: 1.5;
}
.kb-assignee { color: #8d8da3; }

/* Overlay row (execution state) */
.kb-overlay-row { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; margin-top: 7px; font-size: 9px; }
.kb-claim { display: flex; align-items: center; gap: 4px; font-weight: 600; }
.kb-claim-dot { width: 8px; height: 8px; border-radius: 50%; box-shadow: 0 0 6px currentColor; }
.kb-blocked-badge {
  font-size: 8px; font-weight: 700; letter-spacing: 0.5px; text-transform: uppercase;
  color: #ff6b6b; background: rgba(255,107,107,0.15); border: 1px solid rgba(255,107,107,0.4);
  padding: 1px 5px; border-radius: 2px;
}
.kb-review-timer {
  font-size: 8px; font-weight: 700; letter-spacing: 0.5px; text-transform: uppercase;
  color: #74b9ff; background: rgba(116,185,255,0.12); border: 1px solid rgba(116,185,255,0.35);
  padding: 1px 5px; border-radius: 2px;
}

/* Child roll-up */
.kb-rollup { display: flex; align-items: center; gap: 6px; margin-top: 7px; }
.kb-rollup-bar { flex: 1; height: 3px; background: rgba(255,255,255,0.08); border-radius: 2px; overflow: hidden; }
.kb-rollup-fill { height: 100%; background: #00e676; border-radius: 2px; transition: width 0.25s; }
.kb-rollup-text { font: 9px 'JetBrains Mono', monospace; color: rgba(255,255,255,0.45); white-space: nowrap; }
.kb-expand {
  background: none; border: none; color: #8d8da3; cursor: pointer; font-size: 10px;
  padding: 0; font-family: 'JetBrains Mono', monospace;
}
.kb-expand:hover { color: #a29bfe; }
.kb-children { margin-top: 8px; padding-left: 10px; border-left: 1px dashed rgba(108,92,231,0.25); display: flex; flex-direction: column; gap: 6px; }
.kb-child {
  background: rgba(20,20,34,0.7); border: 1px solid rgba(108,92,231,0.12); border-radius: 3px;
  padding: 6px 8px; font-size: 10px; cursor: pointer; transition: border-color 0.15s;
}
.kb-child:hover { border-color: rgba(108,92,231,0.4); }
.kb-child-row { display: flex; align-items: center; gap: 6px; }
.kb-child-state { font-size: 8px; padding: 0 4px; border-radius: 2px; flex-shrink: 0; }
.kb-child-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

/* External link affordance */
.kb-extlink { color: #74b9ff; font-size: 9px; text-decoration: none; }
.kb-extlink:hover { text-decoration: underline; }

/* Detail panel (slide-over) */
.kb-detail-overlay { position: absolute; inset: 0; background: rgba(5,5,10,0.55); z-index: 120; display: flex; justify-content: flex-end; }
.kb-detail {
  width: 440px; max-width: 92%; height: 100%;
  background: rgba(13,13,24,0.99); border-left: 1px solid rgba(108,92,231,0.3);
  box-shadow: -10px 0 40px rgba(0,0,0,0.5); overflow-y: auto; padding: 22px;
  animation: kb-slideIn 0.2s ease-out;
}
.kb-detail::-webkit-scrollbar { width: 5px; }
.kb-detail::-webkit-scrollbar-thumb { background: rgba(108,92,231,0.3); border-radius: 2px; }
.kb-detail-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; margin-bottom: 14px; }
.kb-detail-title { font-size: 15px; font-weight: 700; color: #dfe6e9; line-height: 1.35; }
.kb-detail-close {
  background: none; border: 1px solid rgba(108,92,231,0.25); color: #8d8da3;
  font-size: 14px; width: 26px; height: 26px; cursor: pointer; border-radius: 2px; flex-shrink: 0; line-height: 1;
}
.kb-detail-close:hover { color: #fff; border-color: rgba(108,92,231,0.6); }
.kb-detail-section { margin-bottom: 16px; }
.kb-detail-label { color: #636e72; text-transform: uppercase; font-size: 9px; letter-spacing: 1px; margin-bottom: 5px; }
.kb-detail-desc {
  white-space: pre-wrap; word-break: break-word; font-size: 11px; color: #b2bec3;
  background: rgba(10,10,18,0.5); border-radius: 3px; padding: 10px; line-height: 1.5;
}
.kb-detail-meta { display: flex; flex-wrap: wrap; gap: 8px; }
.kb-detail-pill {
  font-size: 9px; padding: 2px 7px; border-radius: 3px; color: #b2bec3;
  background: rgba(255,255,255,0.05); border: 1px solid rgba(255,255,255,0.1);
}

/* Temporal trail */
.kb-trail { display: flex; flex-direction: column; gap: 0; }
.kb-trail-step { display: flex; gap: 10px; align-items: flex-start; position: relative; padding-bottom: 12px; }
.kb-trail-step:last-child { padding-bottom: 0; }
.kb-trail-dot { width: 9px; height: 9px; border-radius: 50%; margin-top: 3px; flex-shrink: 0; box-shadow: 0 0 6px currentColor; }
.kb-trail-step:not(:last-child)::before {
  content: ''; position: absolute; left: 4px; top: 12px; bottom: 0; width: 1px; background: rgba(108,92,231,0.25);
}
.kb-trail-body { display: flex; flex-direction: column; }
.kb-trail-step-label { font-size: 11px; color: #dfe6e9; }
.kb-trail-step-time { font-size: 9px; color: #636e72; }
.kb-trail-step--blocked .kb-trail-step-label { color: #ff6b6b; }

.kb-comment { background: rgba(10,10,18,0.5); border-radius: 3px; padding: 8px 10px; margin-bottom: 6px; }
.kb-comment-head { display: flex; justify-content: space-between; font-size: 9px; color: #8d8da3; margin-bottom: 3px; }
.kb-comment-agent { color: #a29bfe; font-weight: 600; }
.kb-comment-body { font-size: 11px; color: #b2bec3; white-space: pre-wrap; word-break: break-word; }
.kb-detail-extlink {
  display: inline-flex; align-items: center; gap: 6px; font-size: 11px; color: #74b9ff;
  text-decoration: none; padding: 6px 10px; border: 1px solid rgba(116,185,255,0.3);
  border-radius: 3px; background: rgba(116,185,255,0.06);
}
.kb-detail-extlink:hover { background: rgba(116,185,255,0.14); }

/* Create/edit form (native mode only) */
.kb-form-overlay {
  position: absolute; inset: 0; background: rgba(5,5,10,0.85);
  display: flex; align-items: center; justify-content: center; z-index: 130;
}
.kb-form {
  background: rgba(15,15,26,0.98); border: 1px solid rgba(108,92,231,0.3); border-radius: 4px;
  padding: 24px; width: 560px; max-width: 90%; max-height: 85vh; overflow-y: auto;
  animation: kb-formIn 0.2s ease-out; box-shadow: 0 0 30px rgba(108,92,231,0.15);
}
.kb-form h3 { margin: 0 0 16px; font-size: 13px; color: #6c5ce7; text-transform: uppercase; letter-spacing: 2px; }
.kb-field { margin-bottom: 12px; }
.kb-field label { display: block; font-size: 10px; color: #636e72; text-transform: uppercase; letter-spacing: 1px; margin-bottom: 4px; }
.kb-field input, .kb-field textarea, .kb-field select {
  width: 100%; background: rgba(30,30,50,0.6); border: 1px solid rgba(108,92,231,0.2);
  color: #dfe6e9; font-family: 'JetBrains Mono', monospace; font-size: 12px;
  padding: 7px 10px; border-radius: 2px; outline: none; box-sizing: border-box;
}
.kb-field input:focus, .kb-field textarea:focus, .kb-field select:focus { border-color: rgba(108,92,231,0.6); }
.kb-field textarea { resize: vertical; min-height: 120px; }
.kb-form-btns { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
.kb-form-btn {
  font-family: 'JetBrains Mono', monospace; font-size: 11px; font-weight: 600;
  padding: 6px 16px; border-radius: 2px; cursor: pointer; letter-spacing: 1px;
  text-transform: uppercase; border: 1px solid;
}
.kb-form-btn--cancel { background: transparent; border-color: rgba(108,92,231,0.2); color: #636e72; }
.kb-form-btn--cancel:hover { border-color: rgba(108,92,231,0.4); color: #a29bfe; }
.kb-form-btn--submit { background: rgba(108,92,231,0.2); border-color: rgba(108,92,231,0.5); color: #6c5ce7; }
.kb-form-btn--submit:hover { background: rgba(108,92,231,0.35); }

@media (prefers-reduced-motion: reduce) {
  .kb-card, .kb-card.kb-p0, .kb-card.kb-moved, .kb-card.kb-highlight, .kb-detail, .kb-form { animation: none !important; }
  .kb-card { transition: none; }
}
`;

export class KanbanBoard {
  constructor(container) {
    this.container = container;
    this.tasks = [];
    this.cycles = [];
    this.selectedCycle = "active"; // "active" | "all" | cycle_id
    this.linearMode = false;
    this.collapsed = { backlog: true };
    this.expanded = new Set(); // parent task ids with children shown
    this._detailTaskId = null;

    /** @type {((taskId: string, newStatus: string, agentName?: string) => void)|null} */
    this.onTransition = null;
    /** @type {((data: object) => void)|null} */
    this.onDispatch = null;
    /** @type {((taskId: string, project: string) => void)|null} */
    this.onDelete = null;
    /** @type {((taskId: string, project: string, data: object) => void)|null} */
    this.onEdit = null;
    /** @type {((cycle: string) => void)|null} — fired when the cycle filter changes */
    this.onCycleChange = null;
    /** @type {((taskId: string, project: string) => Promise<object[]>)|null} — fetch progress notes */
    this.fetchProgress = null;

    this._style = document.createElement("style");
    this._style.textContent = KANBAN_STYLES;
    document.head.appendChild(this._style);

    this.root = document.createElement("div");
    this.root.className = "kb-root";
    this.container.appendChild(this.root);

    this._formOverlay = null;
    this._detailOverlay = null;
    this._prevColumns = new Map(); // taskId → column (to detect moves for animation)
    this._timeInterval = setInterval(() => this._updateTimes(), 30000);

    this._render();
  }

  /* ─── Public API ─── */

  setMode(linearMode) {
    if (this.linearMode === linearMode) return;
    this.linearMode = linearMode;
    this._scheduleRender();
  }

  setCycles(cycles) {
    this.cycles = cycles || [];
    this._scheduleRender();
  }

  _fingerprint(arr) {
    let h = 0;
    for (const t of arr) {
      const s = [t.id, t.status, t.linear_state, t.title, t.priority, t.points,
        t.claimed_by, t.assigned_to, t.in_review_at, t.blocked_periods, t.cycle_id,
        t.parent_task_id, t.labels].join("|");
      for (let i = 0; i < s.length; i++) h = ((h << 5) - h + s.charCodeAt(i)) | 0;
    }
    return h;
  }

  setTasks(tasks) {
    const incoming = tasks || [];
    const fp = this._fingerprint(incoming);
    if (fp === this._tasksFP && incoming.length === this.tasks.length) return;
    this.tasks = incoming;
    this._tasksFP = fp;
    if (this._formOverlay) return;
    this._scheduleRender();
  }

  // Apply a single task upsert from SSE without a full reload.
  upsertTask(task) {
    if (!task || !task.id) return;
    const idx = this.tasks.findIndex((t) => t.id === task.id);
    if (idx >= 0) this.tasks[idx] = { ...this.tasks[idx], ...task };
    else this.tasks.push(task);
    this._tasksFP = this._fingerprint(this.tasks);
    if (this._formOverlay) return;
    this._scheduleRender();
    // Keep the open detail panel fresh.
    if (this._detailTaskId === task.id) this._refreshDetail();
  }

  _scheduleRender() {
    if (this._renderRAF) return;
    this._renderRAF = requestAnimationFrame(() => {
      this._renderRAF = null;
      this._render();
    });
  }

  show() { this.root.style.display = "flex"; }
  hide() { this.root.style.display = "none"; }

  highlightTask(taskId) {
    const card = this.root.querySelector(`.kb-card[data-task-id="${taskId}"]`);
    if (!card) return;
    card.scrollIntoView({ behavior: REDUCE_MOTION ? "auto" : "smooth", block: "center" });
    card.classList.add("kb-highlight");
    setTimeout(() => card.classList.remove("kb-highlight"), 2500);
  }

  destroy() {
    clearInterval(this._timeInterval);
    this._closeForm();
    this._closeDetail();
    if (this._style.parentNode) this._style.parentNode.removeChild(this._style);
    if (this.root.parentNode) this.root.parentNode.removeChild(this.root);
  }

  /* ─── Data shaping ─── */

  // Index tasks by id and build parent→children. Returns {byId, childrenOf, roots}.
  _buildHierarchy(tasks) {
    const byId = new Map();
    for (const t of tasks) byId.set(t.id, t);
    const childrenOf = new Map();
    const roots = [];
    for (const t of tasks) {
      if (t.parent_task_id && byId.has(t.parent_task_id)) {
        if (!childrenOf.has(t.parent_task_id)) childrenOf.set(t.parent_task_id, []);
        childrenOf.get(t.parent_task_id).push(t);
      } else {
        roots.push(t);
      }
    }
    return { byId, childrenOf, roots };
  }

  // Roll-up over a parent's children: {done, total}. Done = done/cancelled.
  _rollup(parentId, childrenOf) {
    const kids = childrenOf.get(parentId) || [];
    if (kids.length === 0) return null;
    const done = kids.filter((k) => k.status === "done" || k.status === "cancelled" || columnFor(k) === "done").length;
    return { done, total: kids.length };
  }

  _getGroups() {
    const { childrenOf, roots } = this._buildHierarchy(this.tasks);
    const groups = {};
    for (const c of COLUMNS) groups[c] = [];
    // Only root (top-level) tasks get a column; children render nested.
    for (const t of roots) {
      const col = columnFor(t);
      (groups[col] || groups.todo).push(t);
    }
    // Ordering is already priority→points→dispatched_at from the API; keep stable.
    return { groups, childrenOf };
  }

  /* ─── Rendering ─── */

  _render() {
    this.root.replaceChildren();
    this.root.appendChild(this._buildHeader());

    const board = document.createElement("div");
    board.className = "kb-board";
    const { groups, childrenOf } = this._getGroups();

    const newColumns = new Map();
    for (const col of COLUMNS) {
      const tasks = groups[col] || [];
      for (const t of tasks) newColumns.set(t.id, col);
      board.appendChild(this._renderColumn(col, tasks, childrenOf));
    }
    this.root.appendChild(board);

    // Animate cards that changed column since the last render.
    if (!REDUCE_MOTION && this._prevColumns.size) {
      for (const [id, col] of newColumns) {
        const prev = this._prevColumns.get(id);
        if (prev && prev !== col) {
          const card = board.querySelector(`.kb-card[data-task-id="${id}"]`);
          if (card) {
            card.classList.add("kb-moved");
            setTimeout(() => card.classList.remove("kb-moved"), 1100);
          }
        }
      }
    }
    this._prevColumns = newColumns;
  }

  _buildHeader() {
    const header = document.createElement("div");
    header.className = "kb-header";

    const left = document.createElement("div");
    left.className = "kb-header-left";

    const h2 = document.createElement("h2");
    h2.textContent = "Task Board";
    left.appendChild(h2);

    const modeBadge = document.createElement("span");
    modeBadge.className = "kb-mode-badge " + (this.linearMode ? "kb-mode-badge--linear" : "kb-mode-badge--native");
    modeBadge.textContent = this.linearMode ? "Linear · read-only" : "Native";
    left.appendChild(modeBadge);

    // Cycle name + dates for the selected cycle
    const active = this._currentCycleObj();
    if (active) {
      const info = document.createElement("span");
      info.className = "kb-cycle-info";
      const dates = active.start || active.end
        ? ` · ${this._fmtDate(active.start)} → ${this._fmtDate(active.end)}` : "";
      info.innerHTML = `<b>${esc(active.name || active.id)}</b>${esc(dates)}${active.active ? " · active" : ""}`;
      left.appendChild(info);
    }
    header.appendChild(left);

    const right = document.createElement("div");
    right.className = "kb-header-right";

    if (this.cycles.length > 0) {
      const sel = document.createElement("select");
      sel.className = "kb-cycle-select";
      sel.setAttribute("aria-label", "Cycle filter");
      const opts = [{ value: "active", label: "Active cycle" }, { value: "all", label: "All cycles" }];
      for (const c of this.cycles) {
        opts.push({ value: c.id, label: (c.name || c.id) + (c.active ? " (active)" : "") });
      }
      for (const o of opts) {
        const opt = document.createElement("option");
        opt.value = o.value;
        opt.textContent = o.label;
        if (o.value === this.selectedCycle) opt.selected = true;
        sel.appendChild(opt);
      }
      sel.addEventListener("change", () => {
        this.selectedCycle = sel.value;
        if (this.onCycleChange) this.onCycleChange(sel.value);
      });
      right.appendChild(sel);
    }

    // Create button only in native (writable) mode.
    if (!this.linearMode) {
      const addBtn = document.createElement("button");
      addBtn.className = "kb-add-btn";
      addBtn.textContent = "+";
      addBtn.title = "Create task";
      addBtn.setAttribute("aria-label", "Create task");
      addBtn.addEventListener("click", () => this._showCreateForm());
      right.appendChild(addBtn);
    }

    header.appendChild(right);
    return header;
  }

  _currentCycleObj() {
    if (this.selectedCycle === "all") return null;
    if (this.selectedCycle === "active") return this.cycles.find((c) => c.active) || null;
    return this.cycles.find((c) => c.id === this.selectedCycle) || null;
  }

  _fmtDate(ts) {
    if (!ts) return "?";
    return new Date(ts).toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  _renderColumn(col, tasks, childrenOf) {
    const isCollapsed = !!this.collapsed[col];
    const el = document.createElement("div");
    el.className = "kb-col" + (isCollapsed ? " kb-col--collapsed" : "");
    el.dataset.column = col;

    const hdr = document.createElement("div");
    hdr.className = "kb-col-header";
    hdr.style.color = COLUMN_COLORS[col];
    hdr.innerHTML = `<span>${COLUMN_LABELS[col]}</span><span class="kb-col-count">${tasks.length}</span>`;
    hdr.title = "Click to collapse/expand";
    hdr.addEventListener("click", () => {
      this.collapsed[col] = !this.collapsed[col];
      this._render();
    });
    el.appendChild(hdr);

    const body = document.createElement("div");
    body.className = "kb-col-body";
    if (tasks.length === 0) {
      const empty = document.createElement("div");
      empty.className = "kb-col-empty";
      empty.textContent = "—";
      body.appendChild(empty);
    } else {
      for (const t of tasks) body.appendChild(this._renderCard(t, childrenOf));
    }
    el.appendChild(body);
    return el;
  }

  _renderCard(task, childrenOf) {
    const card = document.createElement("div");
    const blocked = isBlocked(task);
    card.className = "kb-card" + (task.priority === "P0" ? " kb-p0" : "") + (blocked ? " kb-blocked" : "");
    card.dataset.taskId = task.id;
    card.tabIndex = 0;
    card.setAttribute("role", "button");

    const claimedBy = task.claimed_by || task.assigned_to;
    if (claimedBy) {
      card.dataset.claimed = "1";
      card.style.setProperty("--kb-agent-color", agentColor(claimedBy));
    }

    // Top row: key chip, priority, points, time
    const top = document.createElement("div");
    top.className = "kb-card-top";
    let topHTML = "";
    if (task.linear_key) topHTML += `<span class="kb-key">${esc(task.linear_key)}</span>`;
    const prio = task.priority || "P2";
    const pc = PRIORITY_COLORS[prio] || PRIORITY_COLORS.P2;
    topHTML += `<span class="kb-badge" style="color:${pc};background:${pc}25;border:1px solid ${pc}40">${esc(prio)}</span>`;
    if (task.points != null) topHTML += `<span class="kb-points">${esc(task.points)}pt</span>`;
    topHTML += `<span class="kb-time" data-ts="${esc(task.dispatched_at)}">${timeAgo(task.dispatched_at)}</span>`;
    top.innerHTML = topHTML;
    card.appendChild(top);

    // Title
    const title = document.createElement("div");
    title.className = "kb-card-title";
    title.textContent = task.title || "(untitled)";
    card.appendChild(title);

    // Meta: labels + assignee
    const labels = parseLabels(task.labels);
    if (labels.length || task.assignee) {
      const meta = document.createElement("div");
      meta.className = "kb-card-meta";
      for (const l of labels.slice(0, 4)) {
        const name = typeof l === "string" ? l : (l && l.name) || "";
        if (name) meta.innerHTML += `<span class="kb-label">${esc(name)}</span>`;
      }
      if (task.assignee) meta.innerHTML += `<span class="kb-assignee">@${esc(task.assignee)}</span>`;
      card.appendChild(meta);
    }

    // Overlay row: claimed-by agent, blocked badge, in-review timer
    const overlayBits = [];
    if (claimedBy) {
      const color = agentColor(claimedBy);
      overlayBits.push(`<span class="kb-claim" style="color:${color}"><span class="kb-claim-dot" style="background:${color}"></span>${esc(claimedBy)}</span>`);
    }
    if (blocked) overlayBits.push(`<span class="kb-blocked-badge">blocked</span>`);
    if (task.in_review_at && columnFor(task) === "in-review") {
      overlayBits.push(`<span class="kb-review-timer" data-review="${esc(task.in_review_at)}">in review ${minsSince(task.in_review_at)}min</span>`);
    }
    if (overlayBits.length) {
      const row = document.createElement("div");
      row.className = "kb-overlay-row";
      row.innerHTML = overlayBits.join("");
      card.appendChild(row);
    }

    // Child roll-up
    const rollup = this._rollup(task.id, childrenOf);
    if (rollup) {
      const pct = Math.round((rollup.done / rollup.total) * 100);
      const expanded = this.expanded.has(task.id);
      const ru = document.createElement("div");
      ru.className = "kb-rollup";
      ru.innerHTML = `
        <button class="kb-expand" aria-label="Toggle subtasks">${expanded ? "▾" : "▸"}</button>
        <div class="kb-rollup-bar"><div class="kb-rollup-fill" style="width:${pct}%"></div></div>
        <span class="kb-rollup-text">${rollup.done}/${rollup.total}</span>`;
      ru.querySelector(".kb-expand").addEventListener("click", (e) => {
        e.stopPropagation();
        if (this.expanded.has(task.id)) this.expanded.delete(task.id);
        else this.expanded.add(task.id);
        this._render();
      });
      card.appendChild(ru);

      if (expanded) {
        const kidsWrap = document.createElement("div");
        kidsWrap.className = "kb-children";
        for (const kid of childrenOf.get(task.id) || []) {
          kidsWrap.appendChild(this._renderChild(kid));
        }
        card.appendChild(kidsWrap);
      }
    }

    // Card click: detail panel (always read), or external link in linear mode.
    card.addEventListener("click", (e) => {
      if (e.target.closest(".kb-expand")) return;
      this._openDetail(task.id);
    });
    card.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); this._openDetail(task.id); }
    });

    return card;
  }

  _renderChild(kid) {
    const el = document.createElement("div");
    el.className = "kb-child";
    el.dataset.taskId = kid.id;
    const col = columnFor(kid);
    const c = COLUMN_COLORS[col];
    el.innerHTML = `
      <div class="kb-child-row">
        <span class="kb-child-state" style="color:${c};background:${c}22">${COLUMN_LABELS[col]}</span>
        ${kid.linear_key ? `<span class="kb-key">${esc(kid.linear_key)}</span>` : ""}
        <span class="kb-child-title">${esc(kid.title || kid.id)}</span>
      </div>`;
    el.addEventListener("click", (e) => { e.stopPropagation(); this._openDetail(kid.id); });
    return el;
  }

  /* ─── Detail panel ─── */

  _openDetail(taskId) {
    const task = this.tasks.find((t) => t.id === taskId);
    if (!task) return;

    // Linear mode: card click goes to Linear for editing; still show local read panel.
    this._detailTaskId = taskId;
    this._closeDetail();

    const overlay = document.createElement("div");
    overlay.className = "kb-detail-overlay";
    overlay.addEventListener("click", (e) => { if (e.target === overlay) this._closeDetail(); });

    const panel = document.createElement("div");
    panel.className = "kb-detail";
    panel.appendChild(this._buildDetailBody(task));
    overlay.appendChild(panel);

    this._detailEsc = (e) => { if (e.key === "Escape") this._closeDetail(); };
    document.addEventListener("keydown", this._detailEsc);

    this.root.appendChild(overlay);
    this._detailOverlay = overlay;

    // Load comments / progress notes lazily.
    if (this.fetchProgress) {
      this.fetchProgress(task.id, task.project || "default").then((notes) => {
        if (this._detailTaskId !== task.id) return;
        const slot = panel.querySelector(".kb-comments-slot");
        if (slot) slot.replaceWith(this._buildComments(notes || []));
      }).catch(() => {});
    }
  }

  _refreshDetail() {
    if (!this._detailOverlay || !this._detailTaskId) return;
    const task = this.tasks.find((t) => t.id === this._detailTaskId);
    if (!task) { this._closeDetail(); return; }
    const panel = this._detailOverlay.querySelector(".kb-detail");
    const comments = panel.querySelector(".kb-comments");
    panel.replaceChildren(this._buildDetailBody(task));
    if (comments) {
      const slot = panel.querySelector(".kb-comments-slot");
      if (slot) slot.replaceWith(comments);
    }
  }

  _buildDetailBody(task) {
    const frag = document.createDocumentFragment();

    const head = document.createElement("div");
    head.className = "kb-detail-head";
    head.innerHTML = `<div class="kb-detail-title">${task.linear_key ? `<span class="kb-key">${esc(task.linear_key)}</span> ` : ""}${esc(task.title || "(untitled)")}</div>`;
    const close = document.createElement("button");
    close.className = "kb-detail-close";
    close.textContent = "✕";
    close.setAttribute("aria-label", "Close detail");
    close.addEventListener("click", () => this._closeDetail());
    head.appendChild(close);
    frag.appendChild(head);

    // Meta pills
    const meta = document.createElement("div");
    meta.className = "kb-detail-section kb-detail-meta";
    const col = columnFor(task);
    meta.innerHTML += `<span class="kb-detail-pill" style="color:${COLUMN_COLORS[col]}">${COLUMN_LABELS[col]}</span>`;
    meta.innerHTML += `<span class="kb-detail-pill">${esc(task.priority || "P2")}</span>`;
    if (task.points != null) meta.innerHTML += `<span class="kb-detail-pill">${esc(task.points)} pts</span>`;
    if (task.cycle_name) meta.innerHTML += `<span class="kb-detail-pill">${esc(task.cycle_name)}</span>`;
    const claimedBy = task.claimed_by || task.assigned_to;
    if (claimedBy) meta.innerHTML += `<span class="kb-detail-pill" style="color:${agentColor(claimedBy)}">claimed: ${esc(claimedBy)}</span>`;
    if (task.assignee) meta.innerHTML += `<span class="kb-detail-pill">assignee: ${esc(task.assignee)}</span>`;
    for (const l of parseLabels(task.labels)) {
      const name = typeof l === "string" ? l : (l && l.name) || "";
      if (name) meta.innerHTML += `<span class="kb-detail-pill">${esc(name)}</span>`;
    }
    frag.appendChild(meta);

    // Edit affordance / external link
    const editSection = document.createElement("div");
    editSection.className = "kb-detail-section";
    if (this.linearMode && task.external_url) {
      const a = document.createElement("a");
      a.className = "kb-detail-extlink";
      a.href = task.external_url;
      a.target = "_blank";
      a.rel = "noopener";
      a.innerHTML = `↗ Edit in Linear`;
      editSection.appendChild(a);
    } else if (!this.linearMode) {
      const btnRow = document.createElement("div");
      btnRow.style.cssText = "display:flex;gap:8px";
      const editBtn = document.createElement("button");
      editBtn.className = "kb-form-btn kb-form-btn--submit";
      editBtn.textContent = "Edit";
      editBtn.addEventListener("click", () => { this._closeDetail(); this._showEditForm(task); });
      btnRow.appendChild(editBtn);
      const delBtn = document.createElement("button");
      delBtn.className = "kb-form-btn kb-form-btn--cancel";
      delBtn.textContent = "Delete";
      delBtn.addEventListener("click", () => {
        if (this.onDelete) this.onDelete(task.id, task.project || "default");
        this._closeDetail();
      });
      btnRow.appendChild(delBtn);
      editSection.appendChild(btnRow);
    }
    if (editSection.childNodes.length) frag.appendChild(editSection);

    // Description
    if (task.description) {
      const sec = document.createElement("div");
      sec.className = "kb-detail-section";
      sec.innerHTML = `<div class="kb-detail-label">Description</div><div class="kb-detail-desc">${esc(task.description)}</div>`;
      frag.appendChild(sec);
    }
    if (task.result) {
      const sec = document.createElement("div");
      sec.className = "kb-detail-section";
      sec.innerHTML = `<div class="kb-detail-label">Result</div><div class="kb-detail-desc">${esc(task.result)}</div>`;
      frag.appendChild(sec);
    }
    if (task.blocked_reason && isBlocked(task)) {
      const sec = document.createElement("div");
      sec.className = "kb-detail-section";
      sec.innerHTML = `<div class="kb-detail-label">Blocked reason</div><div class="kb-detail-desc" style="border-left:2px solid #ff6b6b">${esc(task.blocked_reason)}</div>`;
      frag.appendChild(sec);
    }

    // Temporal trail
    frag.appendChild(this._buildTrail(task));

    // PR / branch link derived from linear_key
    const branch = branchHint(task);
    if (branch) {
      const sec = document.createElement("div");
      sec.className = "kb-detail-section";
      let html = `<div class="kb-detail-label">PR / branch</div><div class="kb-detail-meta">`;
      html += `<span class="kb-detail-pill">branch: ${esc(branch)}</span>`;
      if (task.external_url) html += `<a class="kb-extlink" href="${esc(task.external_url)}" target="_blank" rel="noopener">↗ ${esc(task.linear_key)}</a>`;
      html += `</div>`;
      sec.innerHTML = html;
      frag.appendChild(sec);
    }

    // Comments slot (filled async)
    const slot = document.createElement("div");
    slot.className = "kb-detail-section kb-comments-slot";
    slot.innerHTML = `<div class="kb-detail-label">Comments</div><div class="kb-detail-desc" style="color:#636e72">Loading…</div>`;
    frag.appendChild(slot);

    return frag;
  }

  _buildTrail(task) {
    const sec = document.createElement("div");
    sec.className = "kb-detail-section";
    const steps = [];
    if (task.dispatched_at) steps.push(["Dispatched", task.dispatched_at, COLUMN_COLORS.todo]);
    if (task.claimed_at) steps.push([`Claimed${task.claimed_by ? " · " + task.claimed_by : ""}`, task.claimed_at, agentColor(task.claimed_by || task.assigned_to)]);
    else if (task.accepted_at) steps.push(["Accepted", task.accepted_at, "#74b9ff"]);
    if (task.started_at) steps.push(["Started", task.started_at, COLUMN_COLORS["in-progress"]]);
    for (const p of parseBlockedPeriods(task.blocked_periods)) {
      if (!p || !p.start) continue;
      const label = p.end ? `Blocked ${fmtTS(p.start)} → ${fmtTS(p.end)}` : `Blocked (open)`;
      steps.push([label, p.start, "#ff6b6b", true]);
    }
    if (task.in_review_at) steps.push(["In review", task.in_review_at, COLUMN_COLORS["in-review"]]);
    if (task.done_at || task.completed_at) steps.push(["Done", task.done_at || task.completed_at, COLUMN_COLORS.done]);

    steps.sort((a, b) => new Date(a[1]).getTime() - new Date(b[1]).getTime());

    let html = `<div class="kb-detail-label">Temporal trail</div><div class="kb-trail">`;
    for (const [label, ts, color, isBlk] of steps) {
      html += `<div class="kb-trail-step${isBlk ? " kb-trail-step--blocked" : ""}">
        <span class="kb-trail-dot" style="color:${color};background:${color}"></span>
        <div class="kb-trail-body">
          <span class="kb-trail-step-label">${esc(label)}</span>
          <span class="kb-trail-step-time">${fmtTS(ts)}</span>
        </div></div>`;
    }
    html += `</div>`;
    sec.innerHTML = html;
    return sec;
  }

  _buildComments(notes) {
    const sec = document.createElement("div");
    sec.className = "kb-detail-section kb-comments";
    let html = `<div class="kb-detail-label">Comments (${notes.length})</div>`;
    if (notes.length === 0) {
      html += `<div class="kb-detail-desc" style="color:#636e72">No comments yet.</div>`;
    } else {
      for (const n of notes) {
        html += `<div class="kb-comment">
          <div class="kb-comment-head"><span class="kb-comment-agent">${esc(n.agent || "agent")}</span><span>${fmtTS(n.created_at)}</span></div>
          <div class="kb-comment-body">${esc(n.note)}</div>
        </div>`;
      }
    }
    sec.innerHTML = html;
    return sec;
  }

  _closeDetail() {
    if (this._detailOverlay) { this._detailOverlay.remove(); this._detailOverlay = null; }
    if (this._detailEsc) { document.removeEventListener("keydown", this._detailEsc); this._detailEsc = null; }
    this._detailTaskId = null;
  }

  /* ─── Native create / edit forms ─── */

  _showCreateForm() {
    if (this.linearMode || this._formOverlay) return;
    this._buildForm({
      heading: "Create Task",
      submitLabel: "Create",
      task: null,
      onSubmit: (data) => {
        if (this.onDispatch) this.onDispatch(data);
      },
    });
  }

  _showEditForm(task) {
    if (this.linearMode || this._formOverlay) return;
    this._buildForm({
      heading: "Edit Task",
      submitLabel: "Save",
      task,
      onSubmit: (data) => {
        if (this.onEdit) this.onEdit(task.id, task.project || "default", data);
      },
    });
  }

  _buildForm({ heading, submitLabel, task, onSubmit }) {
    const overlay = document.createElement("div");
    overlay.className = "kb-form-overlay";

    const form = document.createElement("div");
    form.className = "kb-form";
    const t = task || {};
    const statusOptions = ["pending", "accepted", "in-progress", "in-review", "done", "blocked"];
    form.innerHTML = `
      <h3>${esc(heading)}</h3>
      ${task ? "" : `<div class="kb-field"><label>Profile</label><input type="text" name="profile" placeholder="profile-slug" autocomplete="off" /></div>`}
      <div class="kb-field"><label>Title</label><input type="text" name="title" value="${esc(t.title || "")}" autocomplete="off" /></div>
      <div class="kb-field"><label>Description</label><textarea name="description">${esc(t.description || "")}</textarea></div>
      <div class="kb-field"><label>Priority</label>
        <select name="priority">
          ${["P0", "P1", "P2", "P3"].map((p) => `<option value="${p}"${(t.priority || "P2") === p ? " selected" : ""}>${p}</option>`).join("")}
        </select>
      </div>
      ${task ? `<div class="kb-field"><label>Status</label><select name="status">${statusOptions.map((s) => `<option value="${s}"${t.status === s ? " selected" : ""}>${s}</option>`).join("")}</select></div>` : ""}
      <div class="kb-field"><label>Parent task ID (optional)</label><input type="text" name="parent_task_id" value="${esc(t.parent_task_id || "")}" placeholder="parent-task-uuid" autocomplete="off" /></div>
      <div class="kb-form-btns">
        <button class="kb-form-btn kb-form-btn--cancel" type="button">Cancel</button>
        <button class="kb-form-btn kb-form-btn--submit" type="button">${esc(submitLabel)}</button>
      </div>`;

    form.querySelector(".kb-form-btn--cancel").addEventListener("click", () => this._closeForm());
    form.querySelector(".kb-form-btn--submit").addEventListener("click", () => {
      const get = (n) => { const el = form.querySelector(`[name="${n}"]`); return el ? el.value.trim() : ""; };
      const title = get("title");
      if (!title) return;
      const data = {
        title,
        description: get("description"),
        priority: get("priority") || "P2",
      };
      if (!task) {
        const profile = get("profile");
        if (!profile) return;
        data.profile = profile;
      } else {
        const status = get("status");
        if (status) data.status = status;
      }
      const parent = get("parent_task_id");
      if (parent) data.parent_task_id = parent;
      onSubmit(data);
      this._closeForm();
    });

    overlay.addEventListener("click", (e) => { if (e.target === overlay) this._closeForm(); });
    this._formEsc = (e) => { if (e.key === "Escape") this._closeForm(); };
    document.addEventListener("keydown", this._formEsc);

    overlay.appendChild(form);
    this.root.appendChild(overlay);
    this._formOverlay = overlay;
    requestAnimationFrame(() => { const i = form.querySelector("input"); if (i) i.focus(); });
  }

  _closeForm() {
    if (this._formOverlay) { this._formOverlay.remove(); this._formOverlay = null; }
    if (this._formEsc) { document.removeEventListener("keydown", this._formEsc); this._formEsc = null; }
    this._render();
  }

  /* ─── Live time updates ─── */

  _updateTimes() {
    this.root.querySelectorAll(".kb-time[data-ts]").forEach((el) => {
      el.textContent = timeAgo(el.dataset.ts);
    });
    this.root.querySelectorAll(".kb-review-timer[data-review]").forEach((el) => {
      el.textContent = `in review ${minsSince(el.dataset.review)}min`;
    });
  }
}
