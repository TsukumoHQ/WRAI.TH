// Team — THE message cinema. The project's agents are laid out as a living
// constellation; when a message flows between two of them a luminous comet
// travels along a curved path from sender to receiver, drawing a glowing trail,
// dropping a mono caption near the receiver, and appending to a live transcript.
// Idle agents breathe. Each node carries an activity-heat ring (messages last
// hour). The glow here is the ONE permitted indulgence — everything else stays
// flat. prefers-reduced-motion → comets become instant arrival pings.
import {
  colorFor, initialFor, fmtAgo, prefersReducedMotion, isoSince,
} from './api.js';

const NS = 'http://www.w3.org/2000/svg';
const HEAT_WINDOW = 3600e3;       // 1h
const MAX_TRANSCRIPT = 40;
const MAX_CONCURRENT = 28;

export function initTeam(root, ctx) {
  const esc = ctx.esc;
  const stage = root.querySelector('#stage');
  const svg = root.querySelector('#stageWires');
  const pulsesG = root.querySelector('#stagePulses');
  const linksG = root.querySelector('#stageLinks');
  const nodesEl = root.querySelector('#stageNodes');
  const captionsEl = root.querySelector('#stageCaptions');
  const emptyEl = root.querySelector('#stageEmpty');
  const transcriptBody = root.querySelector('#transcriptBody');
  const transcriptMeta = root.querySelector('#transcriptMeta');

  let agents = [];
  let nameSet = new Set();
  const pos = new Map();           // name → {nx, ny} normalized
  const nodeEl = new Map();        // name → element
  const heat = new Map();          // name → [ts, ...]
  let transcript = [];             // {from, to, line, ts}
  let loadedFor = null;
  let W = 0, H = 0;
  let live = 0;
  let token = 0;
  let ro = null;
  let heatSweep = null;

  const reduce = () => prefersReducedMotion();

  /* ----------------------------- data ----------------------------- */
  async function load() {
    const project = ctx.selection;
    const t = ++token;
    skeleton();
    let list = [];
    try { list = await ctx.api.agents(project); } catch (_) { list = []; }
    if (t !== token) return;
    agents = Array.isArray(list) ? list : [];
    nameSet = new Set(agents.map((a) => a.name));
    loadedFor = project;
    computeLayout();
    renderNodes();
    measure();
    placeNodes();

    // Seed recent flow (transcript + heat) — no animation for history.
    heat.clear();
    transcript = [];
    try {
      const msgs = await ctx.api.messagesLatest(project, isoSince(3 * 3600e3));
      if (t !== token) return;
      if (Array.isArray(msgs)) {
        const sorted = msgs.slice().sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
        for (const m of sorted) {
          const ts = Date.parse(m.created_at) || Date.now();
          const line = m.subject || excerpt(m.content);
          recordHeat(m.from, ts); for (const to of resolveTargets(m.to)) recordHeat(to, ts);
          transcript.unshift({ from: m.from, to: m.to, line, ts });
        }
        transcript = transcript.slice(0, MAX_TRANSCRIPT);
      }
    } catch (_) { /* transcript stays empty */ }
    renderTranscript();
    refreshHeat();
    toggleEmpty();
  }

  const excerpt = (s) => String(s || '').replace(/\s+/g, ' ').trim().slice(0, 80);

  /* --------------------------- layout ----------------------------- */
  // Depth via reports_to chains (edges where both endpoints exist). Leaders sit
  // at the top; each level fans across a gentle arc → a constellation, not a grid.
  function computeLayout() {
    pos.clear();
    if (!agents.length) return;
    const byName = new Map(agents.map((a) => [a.name, a]));
    const depthOf = (a, seen = new Set()) => {
      const rt = a.reports_to;
      if (!rt || !byName.has(rt) || seen.has(a.name)) return 0;
      seen.add(a.name);
      return 1 + depthOf(byName.get(rt), seen);
    };
    const levels = new Map();
    for (const a of agents) {
      const d = depthOf(a);
      if (!levels.has(d)) levels.set(d, []);
      levels.get(d).push(a);
    }
    const depths = [...levels.keys()].sort((x, y) => x - y);
    const L = depths.length;
    depths.forEach((d, li) => {
      const row = levels.get(d).sort(rank);
      const n = row.length;
      row.forEach((a, i) => {
        const frac = n === 1 ? 0.5 : i / (n - 1);
        const x = 0.12 + frac * 0.76;
        let y;
        if (L === 1) {
          // single tier → wide smile arc
          y = 0.30 + Math.sin(frac * Math.PI) * 0.34;
        } else {
          const base = 0.17 + li / (L - 1) * 0.66;
          y = base - Math.sin(frac * Math.PI) * 0.045;
        }
        pos.set(a.name, { nx: x, ny: y });
      });
    });
  }
  // Leaders / executives first, then by heat-ish role, then name.
  function rank(a, b) {
    const ax = (a.is_executive || (a.teams && a.teams.length)) ? 1 : 0;
    const bx = (b.is_executive || (b.teams && b.teams.length)) ? 1 : 0;
    return bx - ax || a.name.localeCompare(b.name);
  }

  function measure() {
    const r = stage.getBoundingClientRect();
    W = r.width; H = r.height;
    svg.setAttribute('viewBox', `0 0 ${Math.max(1, W)} ${Math.max(1, H)}`);
    drawLinks();
  }
  const centerOf = (name) => {
    const p = pos.get(name);
    return p ? { x: p.nx * W, y: p.ny * H } : null;
  };

  /* --------------------------- nodes ------------------------------ */
  function renderNodes() {
    nodeEl.clear();
    if (!agents.length) { nodesEl.innerHTML = ''; return; }
    nodesEl.innerHTML = agents.map(nodeHTML).join('');
    nodesEl.querySelectorAll('.agent-node').forEach((el) => {
      nodeEl.set(el.dataset.name, el);
      const open = () => openAgentSheet(el.dataset.name);
      el.addEventListener('click', open);
      el.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); } });
    });
  }

  /* ---------------- agent sheet: identity + custom avatar ---------------- */
  function openAgentSheet(name) {
    const a = agents.find((x) => x.name === name);
    if (!a) return;
    const el = document.createElement('div');
    el.className = 'agent-sheet';
    el.innerHTML = `
      <header class="as-head">
        <span class="an-avatar as-avatar" style="--c:${colorFor(a.name)}">${a.avatar_url ? `<img class="an-img" src="${esc(a.avatar_url)}" alt="">` : esc(initialFor(a.name))}</span>
        <div><h2>${esc(a.name)}</h2><p class="as-role">${esc(a.role || a.profile_slug || '')}</p></div>
      </header>
      ${a.description ? `<p class="as-desc">${esc(a.description)}</p>` : ''}
      <label class="as-field">
        <span>avatar — url image / gif / meme</span>
        <input type="url" class="as-url" placeholder="https://…/avatar.gif" value="${esc(a.avatar_url || '')}">
      </label>
      <div class="as-preview" aria-hidden="true"></div>
      <div class="as-actions">
        <button class="as-save">enregistrer</button>
        <button class="as-clear" ${a.avatar_url ? '' : 'hidden'}>retirer</button>
        <span class="as-status" role="status"></span>
      </div>`;
    const input = el.querySelector('.as-url');
    const preview = el.querySelector('.as-preview');
    const status = el.querySelector('.as-status');
    const renderPreview = () => {
      const u = input.value.trim();
      preview.innerHTML = u ? `<img src="${esc(u)}" alt="" onerror="this.parentNode.textContent='image introuvable'">` : '';
    };
    input.addEventListener('input', renderPreview);
    renderPreview();
    const save = async (url) => {
      status.textContent = '…';
      try {
        await ctx.api.setAgentAvatar(ctx.selection, a.name, url);
        a.avatar_url = url || null;
        renderNodes(); placeNodes();
        status.textContent = url ? 'ok' : 'retiré';
        setTimeout(() => ctx.closeSheet(), 350);
      } catch (e) { status.textContent = 'erreur : ' + e.message; }
    };
    el.querySelector('.as-save').addEventListener('click', () => save(input.value.trim()));
    el.querySelector('.as-clear').addEventListener('click', () => save(''));
    ctx.openSheet(el);
  }
  function nodeHTML(a) {
    const state = a.online ? (a.activity && a.activity !== 'idle' ? 'active' : 'online') : 'idle';
    const role = a.profile_slug || shortRole(a.role);
    return `<div class="agent-node" data-name="${esc(a.name)}" data-state="${state}" style="--c:${colorFor(a.name)}" tabindex="0" aria-label="${esc(a.name)}${role ? ' · ' + esc(role) : ''}">
      <span class="an-orbit" aria-hidden="true"></span>
      <span class="an-heat" aria-hidden="true"></span>
      <span class="an-avatar">${a.avatar_url ? `<img class="an-img" src="${esc(a.avatar_url)}" alt="" loading="lazy" onerror="this.remove()">` : esc(initialFor(a.name))}<span class="an-status" aria-hidden="true"></span></span>
      <span class="an-label"><span class="an-name">${esc(a.name)}</span>${role ? `<span class="an-role">${esc(role)}</span>` : ''}</span>
    </div>`;
  }
  const shortRole = (r) => String(r || '').split(/[.—-]/)[0].trim().split(/\s+/).slice(0, 3).join(' ');

  function placeNodes() {
    for (const [name, p] of pos) {
      const el = nodeEl.get(name);
      if (el) { el.style.left = (p.nx * 100).toFixed(2) + '%'; el.style.top = (p.ny * 100).toFixed(2) + '%'; }
    }
  }

  // Faint relationship links (reports_to) behind the nodes.
  function drawLinks() {
    if (!agents.length || !W) { linksG.innerHTML = ''; return; }
    const byName = new Map(agents.map((a) => [a.name, a]));
    let html = '';
    for (const a of agents) {
      if (a.reports_to && byName.has(a.reports_to)) {
        const c = centerOf(a.name), pmt = centerOf(a.reports_to);
        if (c && pmt) html += `<path d="${curve(pmt, c)}" class="org-link" />`;
      }
    }
    linksG.innerHTML = html;
  }

  /* --------------------------- heat ------------------------------- */
  function recordHeat(name, ts) {
    if (!name || !nameSet.has(name)) return;
    if (!heat.has(name)) heat.set(name, []);
    heat.get(name).push(ts);
  }
  function refreshHeat() {
    const now = Date.now();
    let max = 1;
    const counts = new Map();
    for (const [name, arr] of heat) {
      const fresh = arr.filter((t) => now - t < HEAT_WINDOW);
      heat.set(name, fresh);
      counts.set(name, fresh.length);
      if (fresh.length > max) max = fresh.length;
    }
    for (const [name, el] of nodeEl) {
      const n = counts.get(name) || 0;
      el.style.setProperty('--heat', (n / max).toFixed(3));
      el.dataset.heat = n;
    }
  }

  /* --------------------------- pulses ----------------------------- */
  function resolveTargets(to) {
    if (!to) return [];
    if (to === '*') return agents.map((a) => a.name).filter((n) => nameSet.has(n));
    if (to.startsWith('team:')) {
      const slug = to.slice(5);
      return agents.filter((a) => (a.teams || []).some((t) => t.slug === slug)).map((a) => a.name);
    }
    return nameSet.has(to) ? [to] : [];
  }

  function onMessage(evt) {
    const from = evt.agent;
    const ts = Number(evt.ts) || Date.now();
    const line = evt.label || '';
    const rawTo = evt.target || '';
    const targets = resolveTargets(rawTo);

    recordHeat(from, ts);
    for (const to of targets) recordHeat(to, ts);
    refreshHeat();

    transcript.unshift({ from, to: rawTo, line, ts });
    if (transcript.length > MAX_TRANSCRIPT) transcript.length = MAX_TRANSCRIPT;
    renderTranscript(true);
    toggleEmpty();

    if (root.hidden) return;                      // accrue silently when off-screen
    const recv = targets.length ? targets : [];
    if (!recv.length) { pingNode(from); return; }  // unknown receiver → sender shimmer
    for (const to of recv.slice(0, 6)) firePulse(from, to, line);
  }

  function firePulse(from, to, line) {
    const a = centerOf(from), b = centerOf(to);
    if (!a || !b) return;
    if (reduce()) { pingNode(to); showCaption(to, line); return; }
    if (live >= MAX_CONCURRENT) { pingNode(to); showCaption(to, line); return; }
    live++;
    const color = colorFor(from);
    const d = curve(a, b);
    const dist = Math.hypot(b.x - a.x, b.y - a.y);
    const dur = Math.round(620 + Math.min(360, dist * 0.55));

    // glowing trail that draws in behind the comet, then fades
    const wire = el('path', { d, class: 'pulse-wire', stroke: color });
    pulsesG.appendChild(wire);
    const len = wire.getTotalLength ? wire.getTotalLength() : dist;
    wire.style.strokeDasharray = String(len);
    wire.style.strokeDashoffset = String(len);
    wire.animate(
      [{ strokeDashoffset: len, opacity: 0.12 },
       { strokeDashoffset: 0, opacity: 0.5, offset: 0.55 },
       { strokeDashoffset: 0, opacity: 0 }],
      { duration: dur + 260, easing: 'cubic-bezier(.33,0,.2,1)', fill: 'forwards' });

    // comet head travelling along the curve (SMIL animateMotion: start → end).
    // begin="indefinite" + beginElement() so a dynamically-inserted animation
    // runs NOW rather than being judged "already past" on the document timeline.
    const comet = el('circle', { r: 3.2, class: 'pulse-comet', fill: color, filter: 'url(#cometGlow)' });
    const motion = el('animateMotion', { dur: dur + 'ms', path: d, fill: 'freeze', begin: 'indefinite', calcMode: 'spline', keyPoints: '0;1', keyTimes: '0;1', keySplines: '0.4 0 0.25 1' });
    comet.appendChild(motion);
    pulsesG.appendChild(comet);
    try { motion.beginElement(); } catch (_) { /* SMIL unsupported → comet sits at start, trail still draws */ }

    const cleanup = () => { wire.remove(); comet.remove(); live = Math.max(0, live - 1); };
    setTimeout(() => { pingNode(to); showCaption(to, line); cleanup(); }, dur);
  }

  function pingNode(name) {
    const el2 = nodeEl.get(name);
    if (!el2) return;
    el2.classList.remove('ping');
    void el2.offsetWidth;
    el2.classList.add('ping');
  }

  function showCaption(name, line) {
    if (!line) return;
    const c = centerOf(name);
    if (!c) return;
    const cap = document.createElement('div');
    cap.className = 'stage-caption';
    cap.textContent = excerpt(line);
    cap.style.left = (c.x / Math.max(1, W) * 100).toFixed(2) + '%';
    cap.style.top = (c.y / Math.max(1, H) * 100).toFixed(2) + '%';
    captionsEl.appendChild(cap);
    setTimeout(() => cap.remove(), reduce() ? 2200 : 2600);
  }

  /* ------------------------- transcript --------------------------- */
  function renderTranscript(flashFirst = false) {
    transcriptMeta.textContent = transcript.length ? `${transcript.length} exchanges` : 'live';
    if (!transcript.length) {
      transcriptBody.innerHTML = '<div class="empty">No messages yet — they will appear here as agents talk.</div>';
      return;
    }
    transcriptBody.innerHTML = transcript.map((m, i) => {
      const toLabel = m.to === '*' ? 'all' : m.to.startsWith('team:') ? m.to.slice(5) : m.to;
      return `<div class="tr-row${flashFirst && i === 0 ? ' flash' : ''}">
        <span class="tr-discs">
          <span class="tr-disc" style="background:${colorFor(m.from)}" title="${esc(m.from)}">${esc(initialFor(m.from))}</span>
          <span class="tr-arrow" aria-hidden="true">→</span>
          <span class="tr-disc" style="background:${colorFor(toLabel)}" title="${esc(m.to)}">${esc(initialFor(toLabel))}</span>
        </span>
        <span class="tr-line">${m.line ? esc(m.line) : '<i class="text-dim">(no subject)</i>'}</span>
        <span class="tr-ts">${fmtAgo(m.ts)}</span>
      </div>`;
    }).join('');
  }

  function toggleEmpty() {
    if (!agents.length) {
      emptyEl.hidden = false;
      emptyEl.dataset.kind = 'none';
      emptyEl.innerHTML = '<span class="se-title">no agents here yet</span><span class="se-sub">this project has no registered agents</span>';
    } else if (!transcript.length) {
      emptyEl.hidden = false;
      emptyEl.dataset.kind = 'silent';      // constellation stays rendered behind this hint
      emptyEl.innerHTML = '<span class="se-title">agents silencieux</span><span class="se-sub">the constellation is quiet — live messages will fly through</span>';
    } else {
      emptyEl.hidden = true;
    }
  }

  /* ------------------------- helpers ------------------------------ */
  function curve(a, b) {
    const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
    const dx = b.x - a.x, dy = b.y - a.y;
    const dist = Math.hypot(dx, dy) || 1;
    const nx = -dy / dist, ny = dx / dist;            // unit normal
    const bow = Math.min(80, dist * 0.2) * (dx >= 0 ? 1 : -1);
    return `M ${a.x.toFixed(1)} ${a.y.toFixed(1)} Q ${(mx + nx * bow).toFixed(1)} ${(my + ny * bow).toFixed(1)} ${b.x.toFixed(1)} ${b.y.toFixed(1)}`;
  }
  function el(tag, attrs) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function skeleton() {
    nodesEl.innerHTML = '';
    emptyEl.hidden = false;
    emptyEl.innerHTML = '<span class="se-title skel" style="width:160px;height:14px"></span>';
    transcriptBody.innerHTML = '<div class="skel" style="height:34px;margin-bottom:8px"></div>'.repeat(5);
  }

  /* --------------------------- wiring ----------------------------- */
  ctx.onEvent((evt) => { if (String(evt.type || '').startsWith('message')) onMessage(evt); });
  // Invalidate only — the router always calls activate() for the visible page,
  // which reloads when loadedFor no longer matches the selection.
  ctx.onSelection(() => { loadedFor = null; });

  if (window.ResizeObserver) {
    ro = new ResizeObserver(() => { if (!root.hidden && agents.length) { measure(); placeNodes(); } });
    ro.observe(stage);
  }
  heatSweep = setInterval(() => { if (!root.hidden) { refreshHeat(); renderTranscript(); } }, 60000);

  return {
    activate() {
      if (loadedFor !== ctx.selection) { load(); return; }
      // already loaded — the stage now has a real size, so re-measure & place.
      requestAnimationFrame(() => { measure(); placeNodes(); refreshHeat(); });
    },
  };
}
