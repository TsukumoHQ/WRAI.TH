import {
  api, boardForSelection, BUCKETS, isActive,
  colorFor, initialFor, connectStream,
} from './api.js';

const LS_KEY = 'wraith.v2.project';
const $ = (id) => document.getElementById(id);

let projects = [];          // from /api/projects
let selection = 'all';
let disconnect = null;      // SSE teardown
let events = [];            // newest-first, capped
const MAX_EVENTS = 50;

/* ---------------- helpers ---------------- */
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

const fmtNum = (n) => {
  n = Number(n) || 0;
  if (n >= 1_000_000) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
};
const projNames = () => projects.map((p) => p.name);
const sumBy = (k) => projects.reduce((a, p) => a + (Number(p[k]) || 0), 0);
const selProjects = () => selection === 'all' ? projects : projects.filter((p) => p.name === selection);

/* ---------------- header / projects ---------------- */
async function initProjects() {
  projects = await api.projects().catch(() => []);
  const saved = localStorage.getItem(LS_KEY);
  const valid = saved === 'all' || projects.some((p) => p.name === saved);
  selection = valid ? saved : 'all';

  const sel = $('projectSelect');
  const opts = ['<option value="all">All projects</option>']
    .concat(projects.map((p) =>
      `<option value="${esc(p.name)}">${esc(p.name)} · ${(Number(p.total_tasks) || 0)}</option>`));
  sel.innerHTML = opts.join('');
  sel.value = selection;
  sel.addEventListener('change', () => {
    selection = sel.value;
    localStorage.setItem(LS_KEY, selection);
    refresh();
    startStream();
  });
}

/* ---------------- KPI + distribution + load ---------------- */
function renderKPIs(tasks, stats) {
  const done = tasks.filter((t) => t.status === 'done').length;
  const prog = tasks.filter((t) => t.status === 'accepted' || t.status === 'in-progress').length;
  const blocked = tasks.filter((t) => t.status === 'blocked').length;
  const activeAgents = new Set(tasks.filter((t) => isActive(t.status) && t.assigned_to).map((t) => t.assigned_to)).size;
  const tokens = selProjects().reduce((a, p) => a + (Number(p.tokens_24h) || 0), 0);

  const cards = document.querySelectorAll('.kpi');
  setKPI(cards[0], fmtNum(done), `${tasks.length} total · ${tasks.length ? Math.round(done / tasks.length * 100) : 0}% complete`);
  setKPI(cards[1], fmtNum(prog), `${activeAgents} agent${activeAgents === 1 ? '' : 's'} active`);
  setKPI(cards[2], fmtNum(blocked), blocked ? 'needs attention' : 'none blocked');
  setKPI(cards[3], fmtNum(tokens), `${selProjects().length} project${selProjects().length === 1 ? '' : 's'}`);

  // green sparkline from throughput series (single project only)
  const spark = cards[0].querySelector('.kpi-spark');
  const series = stats?.throughput?.series;
  if (spark && Array.isArray(series) && series.length > 1) {
    spark.innerHTML = sparkPath(series.map((p) => Number(p.cumulative_tasks) || 0));
  } else if (spark) { spark.innerHTML = ''; }
}
function setKPI(card, num, sub) {
  card.removeAttribute('data-skel');
  card.querySelector('.kpi-num').textContent = num;
  card.querySelector('.kpi-sub').textContent = sub;
}
function sparkPath(vals) {
  const max = Math.max(...vals), min = Math.min(...vals), span = max - min || 1;
  const n = vals.length;
  const pts = vals.map((v, i) => `${(i / (n - 1) * 100).toFixed(1)},${(26 - (v - min) / span * 24).toFixed(1)}`);
  return `<polyline fill="none" stroke="var(--accent)" stroke-width="1.6" points="${pts.join(' ')}" />`;
}

function renderDist(tasks) {
  const card = $('dist'); card.removeAttribute('data-skel');
  const counts = BUCKETS.map((b) => ({ ...b, n: tasks.filter((t) => b.match(t.status)).length }));
  const total = counts.reduce((a, c) => a + c.n, 0);
  $('distTotal').textContent = `${total} task${total === 1 ? '' : 's'}`;
  if (!total) {
    $('distBar').innerHTML = '';
    $('distLegend').innerHTML = '<span class="empty">No tasks in this view</span>';
    return;
  }
  $('distBar').innerHTML = counts.filter((c) => c.n)
    .map((c) => `<i style="width:${(c.n / total * 100).toFixed(2)}%;background:${c.color}"></i>`).join('');
  $('distLegend').innerHTML = counts
    .map((c) => `<span><i class="ld" style="background:${c.color}"></i>${c.label} <b>${c.n}</b></span>`).join('');
}

function renderLoad(tasks) {
  const card = $('loadCard'); card.removeAttribute('data-skel');
  const map = new Map();
  for (const t of tasks) {
    const a = t.assigned_to;
    if (!a) continue;
    const r = map.get(a) || { agent: a, claimed: 0, blocked: 0 };
    if (t.status === 'blocked') r.blocked++;
    else if (isActive(t.status)) r.claimed++;
    map.set(a, r);
  }
  let rows = [...map.values()];
  const maxLoad = Math.max(1, ...rows.map((r) => r.claimed + r.blocked));
  rows.sort((a, b) => (b.claimed + b.blocked) - (a.claimed + a.blocked) || a.agent.localeCompare(b.agent));
  const active = rows.filter((r) => r.claimed + r.blocked > 0).length;
  $('loadMeta').textContent = `${active}/${rows.length} active`;

  if (!rows.length) { $('loadBody').innerHTML = '<div class="empty">No agents assigned</div>'; return; }
  rows = rows.slice(0, 14);
  $('loadBody').innerHTML = rows.map((r) => {
    const idle = r.claimed + r.blocked === 0;
    const cw = (r.claimed / maxLoad * 100).toFixed(1);
    const bw = (r.blocked / maxLoad * 100).toFixed(1);
    return `<div class="load-row${idle ? ' idle' : ''}">
      <span class="avatar" style="background:${colorFor(r.agent)}">${esc(initialFor(r.agent))}</span>
      <span class="load-name">
        <span class="nm">${esc(r.agent)}</span>
        <span class="load-track"><i class="b-claim" style="width:${cw}%"></i><i class="b-block" style="width:${bw}%"></i></span>
      </span>
      <span class="load-stat"><b>${r.claimed}</b>${r.blocked ? ` · <span class="blk">${r.blocked}⚠</span>` : ''}</span>
    </div>`;
  }).join('');
}

/* ---------------- activity feed ---------------- */
function humanize(e) {
  const type = e.type || '', action = e.action || '';
  const label = e.label || (e.semantic && (e.semantic.title || e.semantic.key)) || '';
  const verbs = {
    'task.claimed': 'claimed', 'task.in_progress': 'started', 'task.in_review': 'sent to review',
    'task.done': 'completed', 'task.blocked': 'was blocked on', 'task.dispatch': 'dispatched',
  };
  if (verbs[type]) return { kind: 'task', verb: verbs[type], label };
  if (type === 'task') {
    const v = { dispatch: 'dispatched', claim: 'claimed', complete: 'completed', block: 'blocked', start: 'started', review: 'reviewed' }[action] || action || 'updated';
    return { kind: 'task', verb: v, label };
  }
  if (type === 'message' || type.startsWith('message')) return { kind: 'msg', verb: 'messaged', label: e.target || label };
  if (type === 'memory' || type.startsWith('memory')) {
    const v = action === 'set' ? 'saved memory' : action === 'search' ? 'searched memory' : 'memory';
    return { kind: 'mem', verb: v, label };
  }
  if (type === 'cycle.digest') return { kind: 'sys', verb: 'cycle digest', label };
  if (type.startsWith('event:')) return { kind: 'sys', verb: type.slice(6), label };
  return { kind: type.split('.')[0] || 'event', verb: action || type, label };
}
function dayKey(ms) {
  const d = new Date(ms), t = new Date(), y = new Date(Date.now() - 864e5);
  if (d.toDateString() === t.toDateString()) return 'Today';
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}
const hhmm = (ms) => new Date(ms).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });

function renderFeed(flashFirst = false) {
  const body = $('feedBody');
  if (!events.length) { body.innerHTML = '<div class="empty">No recent activity — live events will appear here</div>'; return; }
  let html = '', lastDay = null;
  events.forEach((e, i) => {
    const ts = Number(e.ts) || Date.now();
    const dk = dayKey(ts);
    if (dk !== lastDay) { html += `<div class="day-head">${dk}</div>`; lastDay = dk; }
    const h = humanize(e), agent = e.agent || 'system';
    html += `<div class="ev-row${flashFirst && i === 0 ? ' flash' : ''}">
      <span class="ev-disc" style="background:${colorFor(agent)}">${esc(initialFor(agent))}</span>
      <span class="ev-text"><span class="agt">${esc(agent)}</span> <span class="ev-kind">${esc(h.verb)}</span>${h.label ? ` <span class="lbl">${esc(h.label)}</span>` : ''}</span>
      <span class="ev-ts">${hhmm(ts)}</span>
    </div>`;
  });
  body.innerHTML = html;
}
function pushEvent(e) {
  events.unshift(e);
  if (events.length > MAX_EVENTS) events.length = MAX_EVENTS;
  renderFeed(true);
}

/* ---------------- load + stream ---------------- */
function skeletonize() {
  document.querySelectorAll('.kpi').forEach((c) => c.setAttribute('data-skel', ''));
  $('dist').setAttribute('data-skel', '');
  $('loadCard').setAttribute('data-skel', '');
  $('loadBody').innerHTML = skelRows(5, 'load');
  $('distBar').innerHTML = '<i class="skel" style="width:100%"></i>';
  $('distLegend').innerHTML = '';
}
function skelRows(n, kind) {
  const w = kind === 'load' ? '' : '';
  let s = '';
  for (let i = 0; i < n; i++) s += `<div class="load-row"><span class="avatar skel"></span><span class="load-name"><span class="nm skel" style="height:11px;width:${40 + (i * 13) % 50}%"></span><span class="load-track skel"></span></span><span class="load-stat skel" style="height:11px"></span></div>`;
  return s;
}

async function refresh() {
  skeletonize();
  let tasks = [], stats = null;
  try {
    tasks = await boardForSelection(selection, projNames());
  } catch (_) { tasks = []; }
  if (selection !== 'all') stats = await api.stats(selection).catch(() => null);

  renderKPIs(tasks, stats);
  renderDist(tasks);
  renderLoad(tasks);

  // seed feed from recent history
  try {
    const recent = await api.events(selection, MAX_EVENTS);
    if (Array.isArray(recent) && recent.length) {
      events = recent.slice().reverse().slice(0, MAX_EVENTS); // recent endpoint is oldest→newest
      events.sort((a, b) => (Number(b.ts) || 0) - (Number(a.ts) || 0));
    }
  } catch (_) { /* keep existing */ }
  renderFeed(false);
}

function startStream() {
  if (disconnect) { disconnect(); disconnect = null; }
  setLive('connecting');
  disconnect = connectStream(selection, (evt) => {
    if (selection !== 'all' && evt.project && evt.project !== selection) return;
    pushEvent(evt);
  }, setLive);
}
function setLive(state) {
  const el = $('liveStatus');
  el.dataset.state = state;
  $('liveLabel').textContent = state === 'live' ? 'live' : state === 'down' ? 'reconnecting' : 'connecting';
}

/* ---------------- boot ---------------- */
(async function boot() {
  await initProjects();
  await refresh();
  startStream();
  // gentle periodic refresh of metrics (board has no push channel)
  setInterval(() => { if (document.visibilityState === 'visible') refresh(); }, 60000);
})();
