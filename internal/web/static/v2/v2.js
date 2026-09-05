// App shell + hash router for the v2 dashboard.
// Owns: project selection, the single SSE stream, the live indicator, and the
// shared slide-over. Each page (overview here, board/stats/notifications as
// modules) receives a `ctx` and self-subscribes to events + selection changes.
import {
  api, boardForSelection, BUCKETS, isActive,
  colorFor, initialFor, connectStream, fmtAgo,
} from './api.js';
import { initBoard } from './board.js';
import { initStats } from './stats.js';
import { initNotifications } from './notifications.js';
import { initHome } from './home.js';
import { initTeam } from './team.js';
import { initMessages } from './messages.js';
import { initMemory } from './memory.js';
import { initLinearStrip } from './linear.js';
import { initFederation } from './federation.js';
import { initSettings } from './settings.js';

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

let projects = [];
let selection = 'all';
let settings = {};
let disconnect = null;
let streamFor = null;                 // selection the live stream is currently bound to
let current = { view: 'home', project: null, page: 'home' };

const eventSubs = new Set();
const selSubs = new Set();

/* ---------------- shared context handed to pages ---------------- */
const ctx = {
  api, esc, colorFor, initialFor, fmtAgo, BUCKETS, isActive,
  get selection() { return selection; },
  get projects() { return projects; },
  get settings() { return settings; },
  get scope() { return current; },
  currentPage: () => current.page,
  projNames: () => projects.map((p) => p.name),
  isMirror: (p) => {
    if (!settings.linear) return false;
    // Multi-project mirror: any relay project the connector writes to is a
    // read-only SSOT mirror. Prefer the full projects[] set; fall back to the
    // single default project for older settings payloads.
    const set = settings.linear.projects;
    if (Array.isArray(set) && set.length) return set.includes(p);
    return !!(settings.linear.project && p === settings.linear.project);
  },
  // Linear team URL (workspace unknown — lands the logged-in user on Linear).
  linearTeamURL: () => 'https://linear.app/',
  onEvent(fn) { eventSubs.add(fn); return () => eventSubs.delete(fn); },
  onSelection(fn) { selSubs.add(fn); return () => selSubs.delete(fn); },
  openSheet, closeSheet,
  // Single source of truth for the projects list: refetch, update the shared
  // array, and hand it back. Pages (home) read through this instead of calling
  // api.projects() directly, so the router and every page stay in lockstep.
  refreshProjects,
};

/* ============================================================= *
 *  Shared slide-over
 * ============================================================= */
let sheetCloser = null;
function openSheet(node) {
  const sheet = $('sheet'), scrim = $('sheetScrim');
  sheet.innerHTML = '';
  sheet.appendChild(node);
  scrim.hidden = false; sheet.hidden = false;
  requestAnimationFrame(() => { scrim.classList.add('open'); sheet.classList.add('open'); });
  sheetCloser = closeSheet;
  scrim.onclick = closeSheet;
}
function closeSheet() {
  const sheet = $('sheet'), scrim = $('sheetScrim');
  sheet.classList.remove('open'); scrim.classList.remove('open');
  setTimeout(() => { sheet.hidden = true; scrim.hidden = true; sheet.innerHTML = ''; }, 220);
  sheetCloser = null;
}
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && sheetCloser) sheetCloser(); });

/* ============================================================= *
 *  Header: projects + live indicator
 * ============================================================= */
async function initHeader() {
  [projects, settings] = await Promise.all([
    api.projects().catch(() => []),
    api.settings().catch(() => ({})),
  ]);
}

// Refetch the projects rollup and update the shared `projects` array in place.
// On network error we keep the last-known list (never blank the console).
// Returns the current array so callers can use the fresh data directly.
async function refreshProjects() {
  try {
    const list = await api.projects();
    if (Array.isArray(list)) projects = list;
  } catch (_) { /* keep last-known list */ }
  return projects;
}

// (Re)bind the live stream to the active scope. The stream is shared and only
// re-opened when the scope's project actually changes (page-to-page nav inside a
// project keeps the same socket).
function startStream() {
  if (streamFor === selection && disconnect) return;
  if (disconnect) { disconnect(); disconnect = null; }
  streamFor = selection;
  setLive('connecting');
  disconnect = connectStream(selection, (evt) => {
    if (selection !== 'all' && evt.project && evt.project !== selection) return;
    teamTabPulse(evt);
    eventSubs.forEach((fn) => { try { fn(evt); } catch (_) { /* page error isolated */ } });
  }, setLive);
}

// A message in this project while you're elsewhere → pulse the team tab.
function teamTabPulse(evt) {
  if (current.view !== 'project' || current.page === 'team') return;
  if (!String(evt.type || '').startsWith('message')) return;
  const tab = document.querySelector('.tab-team');
  if (!tab) return;
  tab.classList.remove('pulsing');
  void tab.offsetWidth;            // restart the animation
  tab.classList.add('has-traffic', 'pulsing');
}
function setLive(state) {
  $('liveStatus').dataset.state = state;
  $('liveLabel').textContent = state === 'live' ? 'live' : state === 'down' ? 'reconnecting' : 'connecting';
}

/* ============================================================= *
 *  Router — project is the container.
 *    #/                       → project home (bento grid)
 *    #/p/<name>/<page>        → inside a project (overview|board|stats|
 *                               notifications|team)
 *  Any hash is deep-link safe: a full reload re-mounts the same view.
 * ============================================================= */
const PAGES = {
  overview: { init: () => overviewPage(ctx), instance: null },
  board: { init: (el) => initBoard(el, ctx), instance: null },
  messages: { init: (el) => initMessages(el, ctx), instance: null },
  federation: { init: (el) => initFederation(el, ctx), instance: null },
  memory: { init: (el) => initMemory(el, ctx), instance: null },
  stats: { init: (el) => initStats(el, ctx), instance: null },
  notifications: { init: (el) => initNotifications(el, ctx), instance: null },
  team: { init: (el) => initTeam(el, ctx), instance: null },
  settings: { init: (el) => initSettings(el, ctx), instance: null },
};
let homeInstance = null;
let linearStrip = null;

function parseHash() {
  const h = location.hash.replace(/^#\/?/, '');
  if (h === 'messages') return { view: 'global', project: null, page: 'messages' };
  if (h === 'federation') return { view: 'global', project: null, page: 'federation' };
  if (h === 'settings') return { view: 'global', project: null, page: 'settings' };
  if (h.startsWith('p/')) {
    const rest = h.slice(2);
    const i = rest.indexOf('/');
    const project = decodeURIComponent(i === -1 ? rest : rest.slice(0, i));
    const page = i === -1 ? 'overview' : rest.slice(i + 1).split('/')[0];
    return { view: 'project', project, page: PAGES[page] ? page : 'overview' };
  }
  return { view: 'home', project: null, page: 'home' };
}

// Build the per-project tab hrefs for the entered project.
function wireProjectTabs(project) {
  document.querySelectorAll('#projTabs .tab').forEach((t) => {
    t.href = `#/p/${encodeURIComponent(project)}/${t.dataset.route}`;
  });
}

function showHomeChrome() {
  document.body.dataset.scope = 'home';
  $('projId').hidden = true;
  $('projTabs').hidden = true;
  $('linearStrip').hidden = true;
  if (linearStrip) linearStrip.update(null);   // stop the connector pollers
}

function showProjectChrome(project) {
  document.body.dataset.scope = 'project';
  $('projId').hidden = false;
  $('projTabs').hidden = false;
  $('projIdName').textContent = project;
  wireProjectTabs(project);
  const mirror = ctx.isMirror(project);
  const badge = $('projLinearBadge');
  badge.hidden = !mirror;
  if (mirror) {
    $('projLinearTeam').textContent = (settings.linear && settings.linear.team_key) || 'linear';
    badge.href = ctx.linearTeamURL();
  }
  // Linear connector strip — mirror project only.
  if (!linearStrip) linearStrip = initLinearStrip($('linearStrip'), ctx);
  linearStrip.update(project);
}

/* ---- "project not found" state — a visible dead-end, never a silent bounce ---- */
// The panel is a router-owned concern, built here so the deep-link fix stays
// contained to v2.js. It mounts as a .page inside #view, so the normal
// page-visibility loop below hides it again the moment you navigate anywhere real.
let notFoundEl = null;
function ensureNotFound() {
  if (notFoundEl) return notFoundEl;
  const st = document.createElement('style');
  st.textContent =
    '.page-notfound{display:flex;align-items:center;justify-content:center;min-height:60vh;padding:40px}'
    + '.page-notfound[hidden]{display:none}'
    + '.nf-wrap{max-width:420px;text-align:center;background:var(--bg-card);border:1px solid var(--border);border-radius:var(--radius);padding:32px 28px}'
    + '.nf-icon{font-size:34px;line-height:1;color:var(--accent);margin-bottom:14px}'
    + '.nf-title{margin:0 0 8px;font-size:18px;font-weight:600;color:var(--text)}'
    + '.nf-msg{margin:0 0 22px;font-size:13px;line-height:1.5;color:var(--text-muted)}'
    + '.nf-msg b{color:var(--text)}'
    + '.nf-back{display:inline-block;padding:8px 16px;font-size:13px;font-weight:500;color:var(--bg);background:var(--accent);border-radius:var(--radius-sm);text-decoration:none}'
    + '.nf-back:hover{filter:brightness(1.08)}';
  document.head.appendChild(st);
  const el = document.createElement('section');
  el.className = 'page page-notfound';
  el.dataset.page = '__notfound';        // never matches a real route → auto-hidden on nav
  el.hidden = true;
  el.innerHTML =
    '<div class="nf-wrap" role="alert" aria-live="polite">'
    + '<div class="nf-icon" aria-hidden="true">&#9888;</div>'
    + '<h2 class="nf-title">Project not found</h2>'
    + '<p class="nf-msg">No project named “<b class="nf-name"></b>” exists on this relay. It may have been renamed or removed.</p>'
    + '<a class="nf-back" href="#/">&larr; Back to fleet</a>'
    + '</div>';
  (document.getElementById('view') || document.body).appendChild(el);
  notFoundEl = el;
  return el;
}
function showNotFound(name) {
  const el = ensureNotFound();
  showHomeChrome();                                    // strip project tabs / stop pollers
  document.querySelectorAll('.page').forEach((p) => { p.hidden = p !== el; });
  el.querySelector('.nf-name').textContent = name || '(unknown)';
  el.hidden = false;
  // scope stays 'home' (set by showHomeChrome) — home chrome is the right frame
  // for a dead-end, and no CSS keys off an unknown 'notfound' scope.
}

async function route() {
  const next = parseHash();

  // Unknown project in a deep link → verify against a FRESH list (it may have
  // just been created elsewhere), then show a visible not-found state with a way
  // back — never a silent redirect. v2.js owns the one projects list.
  if (next.view === 'project') {
    let known = projects.some((p) => p.name === next.project);
    if (!known) { await refreshProjects(); known = projects.some((p) => p.name === next.project); }
    if (!known) { showNotFound(next.project); return; }
  }

  const newSelection = next.view === 'project' ? next.project : 'all';
  const selectionChanged = newSelection !== selection;
  selection = newSelection;
  current = next;

  // (Re)bind stream + fan out selection changes to mounted pages.
  startStream();
  if (selectionChanged) selSubs.forEach((fn) => { try { fn(selection); } catch (_) { /* isolated */ } });

  // Chrome. Global (fleet messages) borrows the home chrome — no project tabs.
  if (next.view === 'home' || next.view === 'global') showHomeChrome(); else showProjectChrome(next.project);

  // Page visibility.
  document.querySelectorAll('.page').forEach((p) => { p.hidden = p.dataset.page !== next.page; });
  document.querySelectorAll('#projTabs .tab').forEach((t) => {
    const on = t.dataset.route === next.page;
    t.classList.toggle('is-active', on);
    if (on) t.setAttribute('aria-current', 'page'); else t.removeAttribute('aria-current');
  });
  if (next.page === 'team') {
    const tab = document.querySelector('.tab-team');
    if (tab) tab.classList.remove('has-traffic', 'pulsing');
  }

  // Mount + activate.
  if (next.view === 'home') {
    if (!homeInstance) homeInstance = initHome(document.querySelector('.page-home'), ctx) || {};
    if (homeInstance.activate) homeInstance.activate();
    return;
  }
  const entry = PAGES[next.page];
  const el = document.querySelector(`.page[data-page="${next.page}"]`);
  if (!entry.instance) entry.instance = entry.init(el) || {};
  if (entry.instance.activate) entry.instance.activate();
}
window.addEventListener('hashchange', route);

/* ============================================================= *
 *  OVERVIEW page (kept intact from phase 1)
 * ============================================================= */
function overviewPage(ctx) {
  const MAX_EVENTS = 50;
  let events = [];
  const fmtNum = (n) => {
    n = Number(n) || 0;
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  };
  const selProjects = () => selection === 'all' ? projects : projects.filter((p) => p.name === selection);

  function renderKPIs(tasks, stats) {
    const done = tasks.filter((t) => t.status === 'done').length;
    const prog = tasks.filter((t) => t.status === 'accepted' || t.status === 'in-progress').length;
    const blocked = tasks.filter((t) => t.status === 'blocked').length;
    const activeAgents = new Set(tasks.filter((t) => isActive(t.status) && t.assigned_to).map((t) => t.assigned_to)).size;
    const tokens = selProjects().reduce((a, p) => a + (Number(p.tokens_24h) || 0), 0);
    const cards = document.querySelectorAll('[data-page="overview"] .kpi');
    setKPI(cards[0], fmtNum(done), `${tasks.length} total · ${tasks.length ? Math.round(done / tasks.length * 100) : 0}% complete`);
    setKPI(cards[1], fmtNum(prog), `${activeAgents} agent${activeAgents === 1 ? '' : 's'} active`);
    setKPI(cards[2], fmtNum(blocked), blocked ? 'needs attention' : 'none blocked');
    setKPI(cards[3], fmtNum(tokens), `${selProjects().length} project${selProjects().length === 1 ? '' : 's'}`);
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
    const max = Math.max(...vals), min = Math.min(...vals), span = max - min || 1, n = vals.length;
    const pts = vals.map((v, i) => `${(i / (n - 1) * 100).toFixed(1)},${(26 - (v - min) / span * 24).toFixed(1)}`);
    return `<polyline fill="none" stroke="var(--accent)" stroke-width="1.6" points="${pts.join(' ')}" />`;
  }
  function renderDist(tasks) {
    $('dist').removeAttribute('data-skel');
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
    $('loadCard').removeAttribute('data-skel');
    const map = new Map();
    for (const t of tasks) {
      const a = t.assigned_to;
      if (!a) continue;
      const r = map.get(a) || { agent: a, claimed: 0, blocked: 0 };
      if (t.status === 'blocked') r.blocked++; else if (isActive(t.status)) r.claimed++;
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

  function skeletonize() {
    document.querySelectorAll('[data-page="overview"] .kpi').forEach((c) => c.setAttribute('data-skel', ''));
    $('dist').setAttribute('data-skel', '');
    $('loadCard').setAttribute('data-skel', '');
    $('loadBody').innerHTML = skelRows(5);
    $('distBar').innerHTML = '<i class="skel" style="width:100%"></i>';
    $('distLegend').innerHTML = '';
  }
  function skelRows(n) {
    let s = '';
    for (let i = 0; i < n; i++) s += `<div class="load-row"><span class="avatar skel"></span><span class="load-name"><span class="nm skel" style="height:11px;width:${40 + (i * 13) % 50}%"></span><span class="load-track skel"></span></span><span class="load-stat skel" style="height:11px"></span></div>`;
    return s;
  }

  async function refresh() {
    skeletonize();
    let tasks = [], stats = null;
    try { tasks = await boardForSelection(selection, ctx.projNames()); } catch (_) { tasks = []; }
    if (selection !== 'all') stats = await api.stats(selection).catch(() => null);
    renderKPIs(tasks, stats);
    renderDist(tasks);
    renderLoad(tasks);
    try {
      const recent = await api.events(selection, MAX_EVENTS);
      if (Array.isArray(recent) && recent.length) {
        events = recent.slice().reverse().slice(0, MAX_EVENTS);
        events.sort((a, b) => (Number(b.ts) || 0) - (Number(a.ts) || 0));
      }
    } catch (_) { /* keep existing */ }
    renderFeed(false);
  }

  ctx.onEvent((evt) => {
    events.unshift(evt);
    if (events.length > MAX_EVENTS) events.length = MAX_EVENTS;
    renderFeed(true);
  });
  ctx.onSelection(() => { events = []; refresh(); });

  refresh();
  setInterval(() => { if (document.visibilityState === 'visible' && current.page === 'overview') refresh(); }, 60000);
  return {};
}

/* ============================================================= *
 *  Config panel wiring — a global page like messages/federation, but its
 *  container + home entry-point are injected here (settings.js owns only the
 *  form) so the slice touches no static HTML.
 * ============================================================= */
function mountSettingsChrome() {
  const view = $('view');
  if (view && !document.querySelector('.page[data-page="settings"]')) {
    const sec = document.createElement('section');
    sec.className = 'page main page-settings';
    sec.dataset.page = 'settings';
    sec.hidden = true;
    view.appendChild(sec);
  }
  // Home entry-point, alongside the other fleet-comms links.
  const hero = document.querySelector('.home-hero-text');
  if (hero && !hero.querySelector('.fleet-comms-link[href="#/settings"]')) {
    const a = document.createElement('a');
    a.className = 'fleet-comms-link';
    a.href = '#/settings';
    a.textContent = 'configuration — Linear connector & console settings →';
    hero.appendChild(a);
  }
}

/* ============================================================= *
 *  Boot
 * ============================================================= */
(async function boot() {
  mountSettingsChrome();
  await initHeader();
  await route();     // mounts the current view (home by default) + binds the stream
})();
