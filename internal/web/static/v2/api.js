// Data layer for the v2 dashboard. Same-origin REST + SSE. Read-only.

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`${res.status} ${url}`);
  return res.json();
}

export const api = {
  health: () => getJSON('/api/health'),
  projects: () => getJSON('/api/projects'),
  board: (project) =>
    getJSON(`/api/tasks/board?project=${encodeURIComponent(project)}&cycle=active`),
  stats: (project) => getJSON(`/api/stats?project=${encodeURIComponent(project)}`),
  events: (project, limit = 50) =>
    getJSON(`/api/events/recent?project=${encodeURIComponent(project)}&limit=${limit}`),
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
