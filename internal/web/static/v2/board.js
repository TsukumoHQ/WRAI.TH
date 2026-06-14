// Board — the flagship. A live kanban with FLIP-animated column moves, drag on
// native projects, read-only external links on the Linear mirror, and a slide-
// over detail. Subscribes to the shared SSE stream for live card movement.
import {
  COLUMNS, COLUMN_STATUS, columnFor, eventStatus, parseLabels,
  taskAgent, priorityRank, colorFor, initialFor, fmtAgo, fmtDur,
  prefersReducedMotion,
} from './api.js';

export function initBoard(root, ctx) {
  const esc = ctx.esc;
  const boardEl = root.querySelector('#board');
  const distEl = root.querySelector('#boardDist');
  const cycleEl = root.querySelector('#cycleFilter');
  const modeEl = root.querySelector('#boardMode');

  let tasks = [];
  let byId = new Map();
  let cycles = [];
  let cycle = 'active';
  let profiles = [];
  let canEdit = false;
  let loadedFor = null;          // `${selection}|${cycle}` last rendered
  let backlogOpen = false;
  let loadToken = 0;             // guards against out-of-order async loads
  let timerInt = null;

  const selection = () => ctx.selection;
  const reduce = () => prefersReducedMotion();

  /* ---------------- data ---------------- */
  async function load(resetCycle = true) {
    const sel = selection();
    canEdit = sel !== 'all' && !ctx.isMirror(sel);
    const token = ++loadToken;
    boardEl.innerHTML = skeleton();

    // cycles + profiles (single project only)
    if (sel === 'all') { cycles = []; profiles = []; }
    else {
      [cycles, profiles] = await Promise.all([
        ctx.api.cycles(sel).catch(() => []),
        canEdit ? ctx.api.profiles(sel).catch(() => []) : Promise.resolve([]),
      ]);
    }
    if (token !== loadToken) return;
    if (resetCycle) {
      const active = cycles.find((c) => c.active);
      cycle = active ? active.id : (cycles.length ? cycles[0].id : 'active');
    }

    let list = [];
    try {
      if (sel === 'all') list = await aggregate();
      else list = await ctx.api.board(sel, cycle);
    } catch (_) { list = []; }
    if (token !== loadToken) return;

    tasks = Array.isArray(list) ? list.filter((t) => columnFor(t) !== null) : [];
    byId = new Map(tasks.map((t) => [t.id, t]));
    loadedFor = `${sel}|${cycle}`;
    renderCycleFilter();
    renderMode();
    render();
  }

  async function aggregate() {
    const lists = await Promise.all(ctx.projNames().map((p) => ctx.api.board(p, 'active').catch(() => [])));
    const out = [];
    for (const l of lists) if (Array.isArray(l)) out.push(...l);
    return out;
  }

  /* ---------------- column model ---------------- */
  function grouped() {
    const cols = new Map(COLUMNS.map((c) => [c.key, []]));
    for (const t of tasks) { const k = columnFor(t); if (cols.has(k)) cols.get(k).push(t); }
    for (const arr of cols.values()) arr.sort(sortCards);
    return cols;
  }
  function sortCards(a, b) {
    return priorityRank(a.priority) - priorityRank(b.priority) ||
      (Date.parse(b.dispatched_at || 0) - Date.parse(a.dispatched_at || 0));
  }
  const childRollup = (t) => {
    const kids = tasks.filter((k) => k.parent_task_id === t.id);
    if (!kids.length) return null;
    return { done: kids.filter((k) => k.status === 'done').length, total: kids.length };
  };
  const parseDeps = (t) => { try { const a = JSON.parse(t.depends_on || '[]'); return Array.isArray(a) ? a : []; } catch (_) { return []; } };
  // Unresolved dependencies, resolved against the loaded board set. Tasks outside
  // the current cycle aren't loaded → treated as resolved here (the server-side
  // gate stays authoritative).
  const depBlockers = (t) => parseDeps(t).map((id) => byId.get(id)).filter((d) => d && d.status !== 'done' && d.status !== 'cancelled');
  // Known agent names across the loaded board — used for the reassign datalist.
  const agentNames = () => [...new Set(tasks.flatMap((t) => [t.assigned_to, t.claimed_by].filter(Boolean)))].sort();

  /* ---------------- render ---------------- */
  function render() {
    if (root.hidden) return;
    const cols = grouped();
    renderDist(cols);
    let html = '';
    for (const c of COLUMNS) {
      const arr = cols.get(c.key);
      if (c.rail && !arr.length) continue;             // hide empty backlog rail
      const railClosed = c.rail && !backlogOpen;
      const pts = arr.reduce((a, t) => a + (Number(t.points) || 0), 0);
      html += `<section class="col${c.rail ? ' col-rail' : ''}${railClosed ? ' closed' : ''}" data-col="${c.key}" style="--col:${c.color}">
        <header class="col-head"${c.rail ? ' role="button" tabindex="0"' : ''}>
          <span class="col-dot" style="background:${c.color}"></span>
          <span class="col-name">${c.label}</span>
          <span class="col-count">${arr.length}${pts ? ` · ${pts}pt` : ''}</span>
        </header>
        ${c.key === 'todo' && canEdit ? quickAdd() : ''}
        <div class="col-cards">${arr.map(card).join('') || (c.rail ? '' : '<div class="col-empty">—</div>')}</div>
      </section>`;
    }
    boardEl.innerHTML = html || '<div class="empty board-empty">No tasks in this view</div>';
    bindCards();
    startTimers();
  }

  function card(t) {
    const col = columnFor(t);
    const key = t.linear_key;
    const agent = taskAgent(t);
    const labels = parseLabels(t).slice(0, 3);
    const blocked = t.status === 'blocked';
    const roll = childRollup(t);
    const inReview = col === 'in_review' && t.in_review_at;
    const ext = t.source === 'linear' && t.external_url;
    const pr = priorityRank(t.priority);
    return `<article class="kcard p${pr}${blocked ? ' is-blocked' : ''}${ext ? ' is-ext' : ''}" data-id="${esc(t.id)}" data-status="${esc(t.status)}"${canEdit && t.source !== 'linear' ? ' data-drag="1"' : ''} tabindex="0">
      <div class="kcard-top">
        ${key ? `<span class="chip-key">${esc(key)}</span>` : `<span class="chip-native">${esc(t.priority || 'P2')}</span>`}
        ${ext ? '<span class="ext-mark" aria-hidden="true">↗</span>' : ''}
        <span class="kcard-spacer"></span>
        ${agent ? `<span class="kc-avatar" title="${esc(agent)}" style="background:${colorFor(agent)}">${esc(initialFor(agent))}</span>` : ''}
      </div>
      <div class="kcard-title">${esc(t.title || '(untitled)')}</div>
      <div class="kcard-meta">
        ${labels.map((l) => `<span class="lchip">${esc(l)}</span>`).join('')}
        ${roll ? `<span class="rollup" title="${roll.done}/${roll.total} children done">▣ ${roll.done}/${roll.total}</span>` : ''}
        ${(() => { const b = depBlockers(t); return b.length ? `<span class="dep-chip" title="waiting on ${b.length} unfinished task${b.length === 1 ? '' : 's'}">⛓ ${b.length}</span>` : ''; })()}
        ${inReview ? `<span class="review-timer" data-since="${esc(t.in_review_at)}">in review ${fmtAgo(t.in_review_at)}</span>` : ''}
        ${blocked ? `<span class="blocked-badge" title="${esc(t.blocked_reason || 'blocked')}"><span class="blk-dot"></span>blocked</span>` : ''}
      </div>
    </article>`;
  }

  function quickAdd() {
    const opts = profiles.map((p) => `<option value="${esc(p.slug)}">${esc(p.slug)}</option>`).join('');
    return `<form class="quick-add" data-quickadd>
      <input class="qa-input" type="text" placeholder="+ quick add task…" aria-label="New task title" />
      <div class="qa-row">
        <select class="qa-profile" aria-label="Profile">${opts || '<option value="">no profile</option>'}</select>
        <button class="qa-btn" type="submit">add</button>
      </div>
    </form>`;
  }

  function renderDist(cols) {
    const segs = [
      ['todo', 'var(--slate)'], ['in_progress', 'var(--blue)'],
      ['in_review', 'var(--amber)'], ['done', 'var(--accent)'],
    ];
    const blocked = tasks.filter((t) => t.status === 'blocked').length;
    const counts = segs.map(([k, c]) => [cols.get(k).length, c]).concat([[blocked, 'var(--red)']]);
    const total = counts.reduce((a, [n]) => a + n, 0);
    distEl.innerHTML = total
      ? counts.filter(([n]) => n).map(([n, c]) => `<i style="width:${(n / total * 100).toFixed(2)}%;background:${c}" title="${n}"></i>`).join('')
      : '';
  }

  function renderCycleFilter() {
    if (!cycles.length) { cycleEl.hidden = true; cycleEl.innerHTML = ''; return; }
    cycleEl.hidden = false;
    const pills = cycles.map((c) =>
      `<button class="cyc-pill${cycle === c.id ? ' on' : ''}" data-cycle="${esc(c.id)}">${esc(c.name)}${c.active ? ' ●' : ''} <b>${c.count}</b></button>`);
    pills.push(`<button class="cyc-pill${cycle === 'all' ? ' on' : ''}" data-cycle="all">all</button>`);
    cycleEl.innerHTML = pills.join('');
    cycleEl.querySelectorAll('.cyc-pill').forEach((b) => b.addEventListener('click', () => {
      cycle = b.dataset.cycle; load(false);
    }));
  }
  function renderMode() {
    if (selection() === 'all') modeEl.textContent = 'all projects · read-only';
    else if (ctx.isMirror(selection())) modeEl.innerHTML = `<span class="ro-dot"></span>read-only · ${esc(selection())} mirror`;
    else modeEl.innerHTML = '<span class="edit-dot"></span>drag to move · click to open';
  }

  /* ---------------- live in-review timers ---------------- */
  function startTimers() {
    if (timerInt) clearInterval(timerInt);
    const tick = () => boardEl.querySelectorAll('.review-timer').forEach((el) => {
      el.textContent = `in review ${fmtAgo(el.dataset.since)}`;
    });
    if (boardEl.querySelector('.review-timer')) timerInt = setInterval(tick, 30000);
  }

  /* ---------------- FLIP reconcile ---------------- */
  function reconcile(mutate) {
    if (root.hidden || reduce()) { mutate(); render(); return; }
    const first = new Map();
    boardEl.querySelectorAll('.kcard').forEach((el) => first.set(el.dataset.id, el.getBoundingClientRect()));
    mutate();
    render();
    const moved = [];
    boardEl.querySelectorAll('.kcard').forEach((el) => {
      const f = first.get(el.dataset.id);
      const l = el.getBoundingClientRect();
      if (!f) { el.classList.add('enter'); el.addEventListener('animationend', () => el.classList.remove('enter'), { once: true }); return; }
      const dx = f.left - l.left, dy = f.top - l.top;
      if (dx || dy) { el.style.transform = `translate(${dx}px,${dy}px)`; el.style.transition = 'none'; moved.push(el); }
    });
    requestAnimationFrame(() => requestAnimationFrame(() => {
      moved.forEach((el) => {
        el.style.transition = 'transform .24s cubic-bezier(.22,.61,.36,1)';
        el.style.transform = '';
        el.classList.add('settling');
        el.addEventListener('transitionend', () => { el.style.transition = ''; el.classList.remove('settling'); }, { once: true });
      });
    }));
  }

  /* ---------------- SSE live updates ---------------- */
  function onEvent(evt) {
    if (!String(evt.type || '').startsWith('task')) return;
    const sem = evt.semantic || {};
    const id = sem.task_id;
    const status = eventStatus(evt);
    if (id && byId.has(id) && status) {
      const t = byId.get(id);
      if (t.status === status) return;
      reconcile(() => {
        t.status = status;
        if (status === 'in-review') t.in_review_at = t.in_review_at || new Date().toISOString();
        if (status === 'done') t.done_at = t.done_at || new Date().toISOString();
        if (status === 'blocked') t.blocked_reason = sem.reason || t.blocked_reason;
      });
    } else if (status === 'pending' || evt.action === 'dispatch') {
      // a new card appeared — refetch lightly, then fade it in via FLIP
      scheduleRefetch();
    }
  }
  let refetchT = null;
  function scheduleRefetch() {
    if (refetchT) return;
    refetchT = setTimeout(async () => {
      refetchT = null;
      const sel = selection();
      const list = await (sel === 'all' ? aggregate() : ctx.api.board(sel, cycle)).catch(() => null);
      if (!Array.isArray(list)) return;
      reconcile(() => {
        tasks = list.filter((t) => columnFor(t) !== null);
        byId = new Map(tasks.map((t) => [t.id, t]));
      });
    }, 600);
  }

  /* ---------------- interactions ---------------- */
  function bindCards() {
    boardEl.querySelectorAll('.col-rail .col-head').forEach((h) => {
      const toggle = () => { backlogOpen = !backlogOpen; render(); };
      h.addEventListener('click', toggle);
      h.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } });
    });
    const qa = boardEl.querySelector('[data-quickadd]');
    if (qa) qa.addEventListener('submit', onQuickAdd);
    boardEl.querySelectorAll('.kcard').forEach((el) => {
      el.addEventListener('click', (e) => { if (!el.dataset.dragging) openCard(el.dataset.id); });
      el.addEventListener('keydown', (e) => { if (e.key === 'Enter') openCard(el.dataset.id); });
      if (el.dataset.drag) attachDrag(el);
    });
  }

  function openCard(id) {
    const t = byId.get(id);
    if (!t) return;
    if (t.source === 'linear' && t.external_url) { window.open(t.external_url, '_blank', 'noopener'); return; }
    openDetail(t);
  }

  async function onQuickAdd(e) {
    e.preventDefault();
    const form = e.currentTarget;
    const input = form.querySelector('.qa-input');
    const title = input.value.trim();
    if (!title) return;
    const profile = form.querySelector('.qa-profile').value || (profiles[0] && profiles[0].slug);
    const btn = form.querySelector('.qa-btn');
    btn.disabled = true; btn.textContent = '…';
    try {
      const created = await ctx.api.dispatchTask({ project: selection(), profile, title, priority: 'P2' });
      input.value = '';
      if (created && created.id) reconcile(() => { tasks.push(created); byId.set(created.id, created); });
    } catch (err) {
      input.classList.add('qa-error'); setTimeout(() => input.classList.remove('qa-error'), 1200);
    } finally { btn.disabled = false; btn.textContent = 'add'; }
  }

  /* ---------------- drag (native) ---------------- */
  function attachDrag(el) {
    el.addEventListener('pointerdown', (e) => {
      if (e.button !== 0) return;
      const startX = e.clientX, startY = e.clientY;
      let ghost = null, started = false, overCol = null;
      const id = el.dataset.id;

      const move = (ev) => {
        if (!started) {
          if (Math.hypot(ev.clientX - startX, ev.clientY - startY) < 6) return;
          started = true; el.dataset.dragging = '1';
          const r = el.getBoundingClientRect();
          ghost = el.cloneNode(true);
          ghost.classList.add('drag-ghost');
          ghost.style.width = r.width + 'px';
          ghost.style.left = r.left + 'px'; ghost.style.top = r.top + 'px';
          ghost.dataset.dx = (ev.clientX - r.left); ghost.dataset.dy = (ev.clientY - r.top);
          document.body.appendChild(ghost);
          el.classList.add('drag-src');
        }
        ghost.style.left = (ev.clientX - ghost.dataset.dx) + 'px';
        ghost.style.top = (ev.clientY - ghost.dataset.dy) + 'px';
        const colEl = document.elementFromPoint(ev.clientX, ev.clientY)?.closest('.col');
        if (colEl !== overCol) {
          boardEl.querySelectorAll('.col.drop').forEach((c) => c.classList.remove('drop'));
          overCol = colEl && colEl.dataset.col !== 'backlog' ? colEl : null;
          if (overCol) overCol.classList.add('drop');
        }
      };
      const up = () => {
        document.removeEventListener('pointermove', move);
        document.removeEventListener('pointerup', up);
        if (ghost) ghost.remove();
        boardEl.querySelectorAll('.col.drop').forEach((c) => c.classList.remove('drop'));
        el.classList.remove('drag-src');
        if (started && overCol) {
          const target = overCol.dataset.col;
          dropTo(id, target);
        }
        setTimeout(() => { delete el.dataset.dragging; }, 0);
      };
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
    });
  }

  async function dropTo(id, colKey) {
    const t = byId.get(id);
    if (!t) return;
    if (columnFor(t) === colKey) return;
    const status = COLUMN_STATUS[colKey];
    const prev = t.status;
    reconcile(() => { t.status = status; if (status === 'in-review') t.in_review_at = new Date().toISOString(); if (status === 'done') t.done_at = new Date().toISOString(); });
    try {
      await ctx.api.transition(id, { project: selection(), status, agent: 'user' });
    } catch (err) {
      reconcile(() => { t.status = prev; });
    }
  }

  /* ---------------- detail slide-over ---------------- */
  const STATUSES = [
    { v: 'pending', l: 'pending' }, { v: 'accepted', l: 'accepted' },
    { v: 'in-progress', l: 'in-progress' }, { v: 'in-review', l: 'in-review' },
    { v: 'blocked', l: 'blocked' }, { v: 'done', l: 'done' }, { v: 'cancelled', l: 'cancelled' },
  ];

  async function openDetail(t) {
    const node = document.createElement('div');
    node.className = 'sheet-inner';
    node.innerHTML = detailShell(t);
    node.querySelector('.sheet-close').addEventListener('click', ctx.closeSheet);
    ctx.openSheet(node);
    const notesEl = node.querySelector('.sheet-notes');
    try {
      const notes = await ctx.api.progress(t.id, t.project || selection());
      notesEl.innerHTML = Array.isArray(notes) && notes.length
        ? notes.slice().reverse().map((n) => `<div class="note"><div class="note-head"><span class="kc-avatar sm" style="background:${colorFor(n.agent)}">${esc(initialFor(n.agent))}</span><span>${esc(n.agent)}</span><span class="note-ts">${fmtAgo(n.created_at)} ago</span></div><div class="note-body">${esc(n.note)}</div></div>`).join('')
        : '<div class="empty">No progress notes</div>';
    } catch (_) { notesEl.innerHTML = '<div class="empty">Could not load notes</div>'; }
    if (canEdit && t.source !== 'linear') {
      wireCommand(node, t);
      loadAudit(node, t);
    }
  }

  // The orchestrator command panel: dependencies, reassign, force status.
  function commandSection(t) {
    const deps = parseDeps(t);
    const depRows = deps.length ? deps.map((id) => {
      const d = byId.get(id);
      const done = d && (d.status === 'done' || d.status === 'cancelled');
      const title = d ? (d.title || id.slice(0, 8)) : id.slice(0, 8);
      return `<li class="dep-row"><span class="dep-stat ${done ? 'ok' : 'open'}" aria-hidden="true"></span><span class="dep-name">${esc(title)}</span><button class="dep-del" data-dep="${esc(id)}" aria-label="Remove dependency ${esc(title)}">✕</button></li>`;
    }).join('') : '<li class="dep-empty">no dependencies</li>';
    const cand = tasks.filter((o) => o.id !== t.id && !deps.includes(o.id))
      .map((o) => `<option value="${esc(o.id)}">${esc(o.title || o.id.slice(0, 8))}</option>`).join('');
    const agents = agentNames().map((a) => `<option value="${esc(a)}"></option>`).join('');
    const statusOpts = STATUSES.map((s) => `<option value="${s.v}"${s.v === t.status ? ' selected' : ''}>${s.l}</option>`).join('');
    return `<div class="sheet-section command">
        <div class="sheet-label">command</div>
        <div class="cmd-block">
          <div class="cmd-h">dependencies</div>
          <ul class="dep-list">${depRows}</ul>
          <select class="cmd-input dep-pick" aria-label="Add a dependency"><option value="">+ add dependency…</option>${cand}</select>
        </div>
        <div class="cmd-block">
          <div class="cmd-h">reassign</div>
          <div class="cmd-row">
            <input class="cmd-input reassign-input" list="cmdAgentList" placeholder="agent name" aria-label="Reassign to agent" />
            <datalist id="cmdAgentList">${agents}</datalist>
            <button class="cmd-btn reassign-btn" type="button">assign</button>
          </div>
        </div>
        <div class="cmd-block">
          <div class="cmd-h">force status</div>
          <div class="cmd-row">
            <select class="cmd-input force-status" aria-label="Force status">${statusOpts}</select>
            <button class="cmd-btn force-btn" type="button">force</button>
          </div>
          <input class="cmd-input force-reason" placeholder="reason (optional)" aria-label="Reason for forcing status" />
        </div>
        <div class="cmd-msg" aria-live="polite"></div>
      </div>`;
  }

  function wireCommand(node, t) {
    const project = t.project || selection();
    const msg = node.querySelector('.cmd-msg');
    const ok = (s) => { msg.textContent = s; msg.dataset.kind = 'ok'; };
    const fail = (e) => { msg.textContent = e.message || String(e); msg.dataset.kind = 'err'; };
    const refresh = async (apply) => {
      try { await apply(); ok('saved'); await load(false); const nt = byId.get(t.id); if (nt) openDetail(nt); }
      catch (e) { fail(e); }
    };
    const deps = parseDeps(t);
    node.querySelector('.dep-pick')?.addEventListener('change', (e) => {
      const id = e.target.value;
      if (id) refresh(() => ctx.api.setDependencies(t.id, project, [...deps, id]));
    });
    node.querySelectorAll('.dep-del').forEach((b) => b.addEventListener('click', () => {
      refresh(() => ctx.api.setDependencies(t.id, project, deps.filter((d) => d !== b.dataset.dep)));
    }));
    node.querySelector('.reassign-btn')?.addEventListener('click', () => {
      const agent = node.querySelector('.reassign-input').value.trim();
      if (agent) refresh(() => ctx.api.reassign(t.id, project, agent));
    });
    node.querySelector('.force-btn')?.addEventListener('click', () => {
      const status = node.querySelector('.force-status').value;
      const reason = node.querySelector('.force-reason').value.trim() || undefined;
      if (status && status !== t.status) refresh(() => ctx.api.transition(t.id, { project, status, reason, force: true }));
    });
  }

  async function loadAudit(node, t) {
    const el = node.querySelector('.sheet-audit');
    if (!el) return;
    try {
      const rows = await ctx.api.audit(t.project || selection(), t.id, 30);
      el.innerHTML = Array.isArray(rows) && rows.length
        ? rows.map((a) => `<div class="audit-row"><span class="audit-act">${esc(a.action.replace(/_/g, ' '))}</span><span class="audit-sum">${esc(a.summary || '')}</span>${a.reason ? `<span class="audit-reason">${esc(a.reason)}</span>` : ''}<span class="audit-ts">${fmtAgo(a.created_at)} ago</span></div>`).join('')
        : '<div class="empty">No actions logged yet</div>';
    } catch (_) { el.innerHTML = '<div class="empty">Could not load audit</div>'; }
  }

  function detailShell(t) {
    const col = columnFor(t);
    const cdef = COLUMNS.find((c) => c.key === col) || {};
    const agent = taskAgent(t);
    const trail = buildTrail(t);
    return `<div class="sheet-head">
        <div class="sheet-chips">
          ${t.linear_key ? `<span class="chip-key">${esc(t.linear_key)}</span>` : ''}
          <span class="chip-native">${esc(t.priority || 'P2')}</span>
          <span class="col-tag" style="color:${cdef.color}"><span class="col-dot" style="background:${cdef.color}"></span>${esc(cdef.label || t.status)}</span>
        </div>
        <button class="sheet-close" aria-label="Close">✕</button>
      </div>
      <h2 class="sheet-title">${esc(t.title || '(untitled)')}</h2>
      <div class="sheet-sub">
        ${agent ? `<span class="kc-avatar sm" style="background:${colorFor(agent)}">${esc(initialFor(agent))}</span><span>${esc(agent)}</span>` : '<span class="text-dim">unassigned</span>'}
        ${t.external_url ? `<a class="sheet-link" href="${esc(t.external_url)}" target="_blank" rel="noopener">open in Linear ↗</a>` : ''}
      </div>
      ${t.blocked_reason ? `<div class="sheet-blocked"><span class="blk-dot"></span>${esc(t.blocked_reason)}</div>` : ''}
      <div class="sheet-section"><div class="sheet-label">timeline</div><ol class="trail">${trail}</ol></div>
      ${t.description ? `<div class="sheet-section"><div class="sheet-label">description</div><div class="sheet-desc">${esc(t.description).slice(0, 4000)}</div></div>` : ''}
      ${canEdit && t.source !== 'linear' ? commandSection(t) : ''}
      <div class="sheet-section"><div class="sheet-label">progress notes</div><div class="sheet-notes"><div class="skel" style="height:14px;width:60%"></div></div></div>
      ${canEdit && t.source !== 'linear' ? '<div class="sheet-section"><div class="sheet-label">audit</div><div class="sheet-audit"><div class="skel" style="height:14px;width:50%"></div></div></div>' : ''}`;
  }

  function buildTrail(t) {
    const steps = [
      ['dispatched', t.dispatched_at, 'var(--text-muted)'],
      ['claimed', t.claimed_at || t.accepted_at, 'var(--blue)'],
      ['started', t.started_at, 'var(--blue)'],
      ['in review', t.in_review_at, 'var(--amber)'],
      ['done', t.done_at || t.completed_at, 'var(--accent)'],
    ].filter(([, ts]) => ts);
    let blockHtml = '';
    try {
      const periods = JSON.parse(t.blocked_periods || '[]');
      if (Array.isArray(periods) && periods.length) {
        blockHtml = periods.map((p) => `<li class="trail-row blk"><span class="trail-dot" style="background:var(--red)"></span><span class="trail-k">blocked</span><span class="trail-v">${p.start ? fmtAgo(p.start) + ' ago' : ''}${p.end ? ` · ${fmtDur((Date.parse(p.end) - Date.parse(p.start)) / 1000)}` : ' · ongoing'}</span></li>`).join('');
      }
    } catch (_) { /* ignore */ }
    return steps.map(([k, ts, color]) =>
      `<li class="trail-row"><span class="trail-dot" style="background:${color}"></span><span class="trail-k">${k}</span><span class="trail-v">${new Date(ts).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</span></li>`).join('') + blockHtml;
  }

  /* ---------------- skeleton ---------------- */
  function skeleton() {
    let cols = '';
    for (const c of COLUMNS) {
      if (c.rail) continue;
      cols += `<section class="col" style="--col:${c.color}"><header class="col-head"><span class="col-dot" style="background:${c.color}"></span><span class="col-name">${c.label}</span></header><div class="col-cards">${'<div class="kcard skel" style="height:74px"></div>'.repeat(3)}</div></section>`;
    }
    return cols;
  }

  /* ---------------- wiring ---------------- */
  ctx.onEvent(onEvent);
  ctx.onSelection(() => { if (!root.hidden) load(true); else loadedFor = null; });

  return {
    activate() {
      if (loadedFor !== `${selection()}|${cycle}` || !boardEl.childElementCount) load(true);
      else render();
    },
  };
}
