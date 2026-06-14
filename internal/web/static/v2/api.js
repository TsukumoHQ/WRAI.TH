// Data layer for the v2 dashboard. Same-origin REST + SSE. Read-only.

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`${res.status} ${url}`);
  return res.json();
}

async function sendJSON(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: body == null ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = '';
    try { const j = await res.json(); msg = j.detail || j.error || ''; } catch (_) { /* ignore */ }
    throw new Error(msg || `${res.status} ${url}`);
  }
  return res.status === 204 ? null : res.json();
}

const q = (o) => Object.entries(o)
  .filter(([, v]) => v != null && v !== '')
  .map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join('&');

export const api = {
  health: () => getJSON('/api/health'),
  projects: () => getJSON('/api/projects'),
  settings: () => getJSON('/api/settings'),
  board: (project, cycle = 'active') =>
    getJSON(`/api/tasks/board?${q({ project, cycle })}`),
  cycles: (project) => getJSON(`/api/cycles?${q({ project })}`),
  profiles: (project) => getJSON(`/api/profiles?${q({ project })}`),
  agents: (project) => getJSON(`/api/agents?${q({ project })}`),
  messagesLatest: (project, since) =>
    getJSON(`/api/messages/latest?${q({ project, since })}`),
  progress: (id, project) =>
    getJSON(`/api/tasks/${encodeURIComponent(id)}/progress?${q({ project })}`),
  stats: (project, cycle) => getJSON(`/api/stats?${q({ project, cycle })}`),
  events: (project, limit = 50) =>
    getJSON(`/api/events/recent?${q({ project, limit })}`),
  // messages (comms)
  messages: (project) => getJSON(`/api/messages?${q({ project })}`),
  messagesAll: () => getJSON('/api/messages/all-projects'),
  sendMessage: (project, to, content, replyTo) =>
    sendJSON('POST', '/api/user-response', { project, to, content, reply_to: replyTo || '' }),
  // memory (curate)
  memories: (project, opts = {}) => getJSON(`/api/memories?${q({ project, ...opts })}`),
  searchMemories: (project, query) => getJSON(`/api/memories/search?${q({ project, q: query })}`),
  setMemory: (body) => sendJSON('POST', '/api/memories', body),
  deleteMemory: (id) => sendJSON('DELETE', `/api/memories/${encodeURIComponent(id)}`),
  resolveMemory: (key, body) => sendJSON('POST', `/api/memories/${encodeURIComponent(key)}/resolve`, body),
  // mutations (native projects only)
  transition: (id, body) =>
    sendJSON('POST', `/api/tasks/${encodeURIComponent(id)}/transition`, body),
  reassign: (id, project, agent) =>
    sendJSON('POST', `/api/tasks/${encodeURIComponent(id)}/reassign`, { project, agent }),
  audit: (project, resource, limit = 50) =>
    getJSON(`/api/audit?${q({ project, resource, limit })}`),
  setAgentAvatar: (project, name, url) =>
    sendJSON('PUT', '/api/agents/avatar', { project, name, url }),
  dispatchTask: (body) => sendJSON('POST', '/api/tasks', body),
  // notifications
  notificationRules: (project) =>
    getJSON(`/api/notification-rules?${q({ project })}`),
  createRule: (body) => sendJSON('POST', '/api/notification-rules', body),
  patchRule: (id, body) =>
    sendJSON('PATCH', `/api/notification-rules/${encodeURIComponent(id)}`, body),
  deleteRule: (id) =>
    sendJSON('DELETE', `/api/notification-rules/${encodeURIComponent(id)}`),
  testFireRule: (id, send = false) =>
    sendJSON('POST', `/api/notification-rules/${encodeURIComponent(id)}/test-fire?${q({ send })}`),
  deliveries: (limit = 50) =>
    getJSON(`/api/notification-deliveries?${q({ limit })}`),
};

// Aggregate a board across many projects in parallel (used for the "All" view,
// since the backend returns empty for project=all).
export async function boardForSelection(selection, projectNames) {
  if (selection !== 'all') return api.board(selection);
  const lists = await Promise.all(
    projectNames.map((p) => api.board(p).catch(() => []))
  );
  const merged = [];
  for (const list of lists) if (Array.isArray(list)) merged.push(...list);
  return merged;
}

// Stable status buckets (authoritative vocabulary: pending, accepted,
// in-progress, in-review, done, blocked).
export const BUCKETS = [
  { key: 'todo', label: 'Todo', color: 'var(--slate)', match: (s) => s === 'pending' },
  { key: 'in_progress', label: 'In Progress', color: 'var(--blue)', match: (s) => s === 'accepted' || s === 'in-progress' },
  { key: 'in_review', label: 'In Review', color: 'var(--amber)', match: (s) => s === 'in-review' },
  { key: 'done', label: 'Done', color: 'var(--accent)', match: (s) => s === 'done' },
  { key: 'blocked', label: 'Blocked', color: 'var(--red)', match: (s) => s === 'blocked' },
];

const ACTIVE = new Set(['accepted', 'in-progress', 'in-review']);
export const isActive = (s) => ACTIVE.has(s);

// ---------------- Board columns ----------------
// Five visible columns. Blocked is a card badge, not a column — blocked cards
// live in In Progress. Cancelled is hidden.
export const COLUMNS = [
  { key: 'backlog', label: 'Backlog', color: 'var(--text-dim)', rail: true },
  { key: 'todo', label: 'Todo', color: 'var(--slate)' },
  { key: 'in_progress', label: 'In Progress', color: 'var(--blue)' },
  { key: 'in_review', label: 'In Review', color: 'var(--amber)' },
  { key: 'done', label: 'Done', color: 'var(--accent)' },
];
// Map a task to its column key (or null to hide).
export function columnFor(task) {
  switch (task.status) {
    case 'cancelled': return null;
    case 'pending': return /backlog/i.test(task.linear_state || '') ? 'backlog' : 'todo';
    case 'accepted':
    case 'in-progress':
    case 'blocked': return 'in_progress';
    case 'in-review': return 'in_review';
    case 'done': return 'done';
    default: return 'todo';
  }
}
// Status to write when a card is dropped into a column (native transition).
export const COLUMN_STATUS = {
  backlog: 'pending', todo: 'pending', in_progress: 'in-progress',
  in_review: 'in-review', done: 'done',
};
// SSE event → resulting task status, so a live event can move a known card.
const EVENT_STATUS = {
  'task.dispatched': 'pending', 'task.dispatch': 'pending', dispatch: 'pending',
  'task.claimed': 'accepted', 'task.claim': 'accepted', claim: 'accepted',
  'task.in_progress': 'in-progress', 'task.start': 'in-progress', start: 'in-progress',
  'task.in_review': 'in-review', 'task.review': 'in-review', review: 'in-review',
  'task.done': 'done', 'task.complete': 'done', complete: 'done',
  'task.blocked': 'blocked', 'task.block': 'blocked', block: 'blocked',
};
// Resolve the status implied by an SSE event (type first, then action).
export function eventStatus(evt) {
  return EVENT_STATUS[evt.type] || EVENT_STATUS[evt.action] || null;
}

export function parseLabels(task) {
  try {
    const v = JSON.parse(task.labels || '[]');
    return Array.isArray(v) ? v.filter(Boolean).map(String) : [];
  } catch (_) { return []; }
}
export const taskAgent = (t) => t.assigned_to || t.assignee || t.claimed_by || '';
export const priorityRank = (p) => {
  const m = String(p || '').toUpperCase().match(/P?([0-3])/);
  return m ? Number(m[1]) : 2;
};

// ---------------- Formatters ----------------
// Compact "time ago" from an epoch-ms or ISO timestamp.
export function fmtAgo(ts) {
  const ms = typeof ts === 'number' ? ts : Date.parse(ts);
  if (!ms) return '';
  let s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return `${Math.floor(s)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}
// Human duration from a count of seconds (for stats).
export function fmtDur(sec) {
  sec = Number(sec) || 0;
  if (sec <= 0) return '—';
  if (sec < 90) return `${Math.round(sec)}s`;
  const m = sec / 60;
  if (m < 90) return `${Math.round(m)}m`;
  const h = m / 60;
  if (h < 48) return `${h.toFixed(h < 10 ? 1 : 0)}h`;
  return `${(h / 24).toFixed(1)}d`;
}

export const prefersReducedMotion = () =>
  window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// Parse a Go duration string ("1m0s", "90s", "1h30m") into milliseconds.
export function parseGoDuration(s) {
  if (typeof s === 'number') return s;
  let ms = 0; const re = /([\d.]+)(h|m|s|ms)/g; let m;
  while ((m = re.exec(String(s || '')))) {
    const v = parseFloat(m[1]);
    ms += v * ({ h: 3600e3, m: 60e3, s: 1e3, ms: 1 }[m[2]] || 0);
  }
  return ms;
}

// ISO timestamp `n` ms before now, in the microsecond format the API expects.
export const isoSince = (ms) =>
  new Date(Date.now() - ms).toISOString().replace('Z', '000Z');

// Deterministic muted disc color from a name.
const PALETTE = ['#4ade80', '#60a5fa', '#fbbf24', '#a78bfa', '#f87171', '#34d399', '#f472b6', '#38bdf8', '#fb923c', '#818cf8'];
export function colorFor(name) {
  let h = 0;
  const s = name || '?';
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}
export const initialFor = (name) => (name || '?').trim().charAt(0).toUpperCase() || '?';

// SSE with exponential backoff. onEvent(MCPEvent), onState('live'|'down').
export function connectStream(project, onEvent, onState) {
  let es = null, closed = false, delay = 1000, timer = null;
  const url = () => `/api/events/stream?project=${encodeURIComponent(project)}`;
  function open() {
    if (closed) return;
    es = new EventSource(url());
    es.onopen = () => { delay = 1000; onState('live'); };
    es.onmessage = (e) => {
      try { onEvent(JSON.parse(e.data)); } catch (_) { /* ignore */ }
    };
    es.onerror = () => {
      onState('down');
      es.close();
      if (closed) return;
      timer = setTimeout(open, delay);
      delay = Math.min(delay * 2, 15000);
    };
  }
  open();
  return () => { closed = true; if (timer) clearTimeout(timer); if (es) es.close(); };
}
