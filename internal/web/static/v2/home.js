// Mission control — the fleet-ops console. Cross-project command center that
// answers, top-to-bottom (urgent first, F-pattern): what's the fleet doing right
// now (golden-signal KPIs), what's the throughput trend (the pulse chart), which
// crews need me (attention rows), and the full roster (fleet cards with live crew
// pips). Per-agent deep-dive stays on the project pages; the home is altitude.
//   data: /api/projects (per-crew rollup) · /api/agents/all (live roster) ·
//   /api/fleet/throughput (persistent daily series) · SSE stream (the ticker).
export function initHome(root, ctx) {
  const esc = ctx.esc;
  const $ = (id) => root.querySelector('#' + id);
  const kpisBox = $('mcKpis');
  const pulseWrap = $('pulseWrap');
  const pulseMeta = $('pulseMeta');
  const ticker = $('fleetTicker');
  const attnSection = $('attnSection');
  const attnList = $('attnList');
  const attnCount = $('attnCount');
  const fleetCount = $('fleetCount');
  const bento = $('bento');

  let projects = [];
  let agentsByProj = new Map();
  let ticks = [];
  let loaded = false;
  const cardByName = new Map();

  const STALE_MS = 24 * 3600e3;
  const fmtNum = (n) => {
    n = Number(n) || 0;
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  };
  const plur = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;

  async function refresh() {
    if (!loaded) skeleton();
    const [pl, al, tp] = await Promise.allSettled([
      ctx.refreshProjects(),           // SSOT: also updates the router's projects list
      ctx.api.agentsAll(),
      ctx.api.fleetThroughput(30),
    ]);
    projects = pl.status === 'fulfilled' && Array.isArray(pl.value) ? pl.value : (ctx.projects || []);
    const agents = al.status === 'fulfilled' && Array.isArray(al.value) ? al.value : [];
    agentsByProj = new Map();
    for (const a of agents) {
      const k = a.project || '';
      if (!agentsByProj.has(k)) agentsByProj.set(k, []);
      agentsByProj.get(k).push(a);
    }
    loaded = true;
    renderKpis(agents);
    renderPulse(tp.status === 'fulfilled' && Array.isArray(tp.value) ? tp.value : []);
    renderFleet();
  }

  /* ---------------- golden-signal KPIs ---------------- */
  function renderKpis(agents) {
    const sum = (k) => projects.reduce((a, p) => a + (Number(p[k]) || 0), 0);
    const total = sum('total_tasks');
    const done = sum('done_tasks');
    const active = sum('active_tasks');
    const blocked = sum('blocked_tasks');
    const online = agents.filter((a) => a.online).length;
    const roster = agents.length;
    const pct = total ? Math.round(done / total * 100) : 0;
    kpisBox.innerHTML = [
      kpi(String(online), `/ ${roster}`, 'agents online', online ? 'good' : 'dim'),
      kpi(String(active), '', 'active tasks', active ? '' : 'dim'),
      kpi(String(blocked), '', 'blocked', blocked ? 'bad' : 'dim'),
      kpi(String(projects.length), '', 'crews', ''),
      kpi(`${pct}%`, '', `${fmtNum(done)} / ${fmtNum(total)} shipped`, ''),
    ].join('');
  }
  const kpi = (num, of, label, cls) =>
    `<div class="mc-kpi${cls ? ' ' + cls : ''}">
      <span class="mck-num">${num}${of ? `<span class="of">${esc(of)}</span>` : ''}</span>
      <span class="mck-label">${esc(label)}</span>
    </div>`;

  /* ---------------- pulse chart — daily throughput ---------------- */
  function renderPulse(buckets) {
    if (!buckets.length) { pulseWrap.innerHTML = '<div class="empty">No throughput yet.</div>'; pulseMeta.textContent = ''; return; }
    const W = 600, H = 84, n = buckets.length, gap = n > 40 ? 1 : 2, bw = (W - gap * (n - 1)) / n;
    const maxV = Math.max(1, ...buckets.map((b) => Math.max(b.done, b.dispatched)));
    const y = (v) => H - 4 - (v / maxV) * (H - 14);
    let bars = '', pts = [];
    buckets.forEach((b, i) => {
      const x = i * (bw + gap);
      const top = y(b.done);
      bars += `<rect x="${x.toFixed(1)}" y="${top.toFixed(1)}" width="${bw.toFixed(1)}" height="${(H - 4 - top).toFixed(1)}" rx="${Math.min(1.5, bw / 3).toFixed(1)}" class="pl-bar${i === n - 1 ? ' today' : ''}"><title>${b.date}: ${b.done} shipped · ${b.dispatched} dispatched</title></rect>`;
      pts.push(`${(x + bw / 2).toFixed(1)},${y(b.dispatched).toFixed(1)}`);
    });
    const area = `<polygon class="pl-area" points="0,${H - 4} ${pts.join(' ')} ${W},${H - 4}" />`;
    const line = `<polyline class="pl-disp" points="${pts.join(' ')}" vector-effect="non-scaling-stroke" />`;
    const base = `<line class="pl-base" x1="0" y1="${H - 4}" x2="${W}" y2="${H - 4}" vector-effect="non-scaling-stroke" />`;
    pulseWrap.innerHTML = `<svg class="pulse-svg" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="Tasks shipped per day over the last 30 days">${base}${area}${bars}${line}</svg>`;
    const ds = buckets.reduce((a, b) => a + b.done, 0);
    const dp = buckets.reduce((a, b) => a + b.dispatched, 0);
    pulseMeta.innerHTML = `<span class="pm-done">${fmtNum(ds)} shipped</span> · <span class="pm-disp">${fmtNum(dp)} dispatched</span>`;
  }

  /* ---------------- attention triage ---------------- */
  function attnReasons(p) {
    const r = [];
    const blocked = Number(p.blocked_tasks) || 0;
    const total = Number(p.total_tasks) || 0;
    const done = Number(p.done_tasks) || 0;
    if (blocked > 0) r.push({ sev: 'red', t: plur(blocked, 'blocked') });
    const la = p.last_activity ? Date.parse(p.last_activity) : 0;
    if (la && total > done && (Date.now() - la) >= STALE_MS) {
      r.push({ sev: 'amber', t: `untouched ${ctx.fmtAgo(la)}` });
    }
    return r;
  }
  const rank = (rs) => rs.reduce((s, r) => s + (r.sev === 'red' ? 100 : 1), 0);

  function renderFleet() {
    cardByName.clear();
    if (!projects.length) {
      attnSection.hidden = true; attnList.innerHTML = '';
      bento.innerHTML = '<div class="empty">No crews yet.</div>';
      fleetCount.textContent = '';
      return;
    }
    const tagged = projects.map((p) => ({ p, reasons: attnReasons(p) }));
    const attn = tagged.filter((x) => x.reasons.length);
    const rest = tagged.filter((x) => !x.reasons.length).map((x) => x.p);

    attn.sort((a, b) =>
      rank(b.reasons) - rank(a.reasons) ||
      (Number(b.p.blocked_tasks) - Number(a.p.blocked_tasks)) ||
      ((Date.parse(a.p.last_activity) || Infinity) - (Date.parse(b.p.last_activity) || Infinity)));

    rest.sort((a, b) => {
      const am = ctx.isMirror(a.name) ? 1 : 0, bm = ctx.isMirror(b.name) ? 1 : 0;
      if (am !== bm) return bm - am;
      return (b.online_count - a.online_count) || (b.active_tasks - a.active_tasks) ||
        (b.total_tasks - a.total_tasks) || a.name.localeCompare(b.name);
    });

    if (attn.length) {
      attnSection.hidden = false;
      attnCount.textContent = String(attn.length);
      const shown = attn.slice(0, 7);
      attnList.innerHTML = shown.map((x) => attnRow(x.p, x.reasons)).join('') +
        (attn.length > shown.length ? `<div class="attn-more">+${attn.length - shown.length} more crew${attn.length - shown.length === 1 ? '' : 's'} need attention</div>` : '');
      wire(attnList);
    } else {
      attnSection.hidden = true; attnList.innerHTML = '';
    }
    bento.innerHTML = rest.map((p) => card(p)).join('');
    fleetCount.textContent = String(rest.length);
    wire(bento);
  }

  function attnRow(p, reasons) {
    const sev = reasons.some((r) => r.sev === 'red') ? 'red' : 'amber';
    const total = Number(p.total_tasks) || 0, done = Number(p.done_tasks) || 0;
    const open = Math.max(0, total - done);
    const chips = reasons.map((r) => `<span class="ar-reason ${r.sev}">${esc(r.t)}</span>`).join('');
    return `<div class="attn-row sev-${sev}" role="link" tabindex="0" data-name="${esc(p.name)}" data-href="#/p/${encodeURIComponent(p.name)}/overview" aria-label="Enter ${esc(p.name)}">
      <span class="ar-dot" aria-hidden="true"></span>
      <span class="ar-proj">${esc(p.name)}</span>
      <span class="ar-reasons">${chips}</span>
      <span class="ar-spacer"></span>
      <span class="ar-meta">${open} open · ${done}/${total}</span>
      <span class="ar-go" aria-hidden="true">↗</span>
    </div>`;
  }

  /* ---------------- fleet cards ---------------- */
  function crewPips(name) {
    const list = (agentsByProj.get(name) || []).slice().sort((a, b) =>
      (b.online ? 1 : 0) - (a.online ? 1 : 0) || (a.status || '').localeCompare(b.status || ''));
    if (!list.length) return '';
    const pip = (a) => {
      const s = a.online ? 'on' : (a.status === 'sleeping' ? 'sleep' : 'off');
      return `<span class="crew-pip ${s}" title="${esc(a.name)} · ${a.online ? 'online' : esc(a.status || 'offline')}"></span>`;
    };
    const pips = list.slice(0, 8).map(pip).join('');
    const more = list.length > 8 ? `<span class="crew-more">+${list.length - 8}</span>` : '';
    return `<div class="pc-crew" aria-hidden="true">${pips}${more}</div>`;
  }

  function segBar(p) {
    const total = Number(p.total_tasks) || 0;
    if (!total) return '<div class="pc-bar" aria-hidden="true"></div>';
    const done = Number(p.done_tasks) || 0, active = Number(p.active_tasks) || 0, blocked = Number(p.blocked_tasks) || 0;
    const w = (n) => (n / total * 100).toFixed(2) + '%';
    return `<div class="pc-bar" role="img" aria-label="${done} done, ${active} active, ${blocked} blocked of ${total}">
      <i class="seg-done" style="width:${w(done)}"></i><i class="seg-active" style="width:${w(active)}"></i><i class="seg-block" style="width:${w(blocked)}"></i>
    </div>`;
  }

  function card(p) {
    const mirror = ctx.isMirror(p.name);
    const total = Number(p.total_tasks) || 0, done = Number(p.done_tasks) || 0, active = Number(p.active_tasks) || 0;
    const online = Number(p.online_count) || 0, agents = Number(p.agent_count) || 0;
    const blocked = Number(p.blocked_tasks) || 0, tokens = Number(p.tokens_24h) || 0;
    const la = p.last_activity || '';
    const live = online > 0 || active > 0;
    const chip = blocked > 0 ? '<span class="pc-chip blk">blocked</span>'
      : online > 0 ? '<span class="pc-chip live">live</span>'
        : active > 0 ? '<span class="pc-chip warm">queued</span>'
          : '<span class="pc-chip idle">idle</span>';
    const meta = blocked > 0 ? `<span class="m-blk">${plur(blocked, 'blocked')}</span>`
      : active > 0 ? `${active} active`
        : la ? `${ctx.fmtAgo(la)} ago`
          : tokens ? `${fmtNum(tokens)} tok`
            : 'idle';
    const linear = mirror
      ? `<a class="pc-linear" href="${esc(ctx.linearTeamURL())}" target="_blank" rel="noopener" title="Mirrored from Linear — open Linear"><span class="lin-diamond" aria-hidden="true">◆</span>${esc((ctx.settings.linear && ctx.settings.linear.team_key) || 'linear')}<span class="lin-arrow" aria-hidden="true">↗</span></a>`
      : '';
    const cls = ['proj-card', mirror ? 'is-mirror' : '', live ? 'is-live' : ''].filter(Boolean).join(' ');
    return `<div class="${cls}" role="link" tabindex="0" data-name="${esc(p.name)}" data-href="#/p/${encodeURIComponent(p.name)}/overview" aria-label="Enter project ${esc(p.name)}">
      <div class="pc-top">
        <span class="pc-name">${esc(p.name)}</span>
        ${chip}
        <span class="pc-spacer"></span>
        ${linear}
      </div>
      <div class="pc-agents">
        <span class="pc-dot${online ? ' on' : ''}" aria-hidden="true"></span>
        <span class="pc-agents-n">${online ? `${online} online` : plur(agents, 'agent')}</span>
        ${online && agents !== online ? `<span class="pc-agents-sub">· ${agents} total</span>` : ''}
        ${crewPips(p.name)}
      </div>
      ${segBar(p)}
      <div class="pc-foot">
        <span class="pc-foot-tasks">${done}<span class="of">/${total}</span> done</span>
        <span class="pc-foot-meta">${meta}</span>
      </div>
    </div>`;
  }

  function wire(grid) {
    grid.querySelectorAll('[data-href]').forEach((el) => {
      if (el.dataset.name) cardByName.set(el.dataset.name, el);
      const enter = () => { location.hash = el.dataset.href; };
      el.addEventListener('click', enter);
      el.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); enter(); }
      });
    });
    grid.querySelectorAll('.pc-linear').forEach((a) => a.addEventListener('click', (e) => e.stopPropagation()));
  }

  /* ---------------- live ticker — "what just landed" ---------------- */
  function humanizeEvt(e) {
    const type = e.type || '', action = e.action || '';
    const label = e.label || (e.semantic && (e.semantic.title || e.semantic.key)) || '';
    const verbs = {
      'task.claimed': 'claimed', 'task.in_progress': 'started', 'task.in_review': 'in review',
      'task.done': 'landed', 'task.blocked': 'blocked', 'task.dispatch': 'dispatched',
    };
    const kindOf = (t, a) => (t === 'task.done' || a === 'complete') ? 'done'
      : (t === 'task.blocked' || a === 'block') ? 'block' : 'task';
    if (verbs[type]) return { verb: verbs[type], label, kind: kindOf(type, action) };
    if (type === 'task') {
      const m = { dispatch: 'dispatched', claim: 'claimed', complete: 'landed', block: 'blocked', start: 'started', review: 'in review' };
      return { verb: m[action] || action || 'updated', label, kind: kindOf(type, action) };
    }
    if (type.startsWith('message')) return { verb: 'messaged', label: e.target || label, kind: 'msg' };
    if (type.startsWith('memory')) return { verb: 'memory', label, kind: 'mem' };
    if (type.startsWith('event:')) return { verb: type.slice(6), label, kind: 'sys' };
    return { verb: action || type, label, kind: 'sys' };
  }

  function renderTicker() {
    if (!ticker) return;
    if (!ticks.length) { ticker.hidden = true; ticker.innerHTML = ''; return; }
    ticker.hidden = false;
    ticker.innerHTML = '<span class="ft-led" aria-hidden="true"></span><span class="ft-tag">live</span>' +
      ticks.slice(0, 16).map((e, i) => {
        const h = humanizeEvt(e), agent = e.agent || 'system', ts = Number(e.ts) || Date.now();
        return `<span class="ft-item${i === 0 ? ' fresh' : ''}">
          <span class="ft-av" style="background:${ctx.colorFor(agent)}">${esc(ctx.initialFor(agent))}</span>
          <span class="ft-agt">${esc(agent)}</span>
          <span class="ft-verb k-${h.kind}">${esc(h.verb)}</span>
          ${h.label ? `<span class="ft-lbl">${esc(h.label)}</span>` : ''}
          ${e.project ? `<span class="ft-proj">${esc(e.project)}</span>` : ''}
          <span class="ft-ago">${ctx.fmtAgo(ts)}</span>
        </span>`;
      }).join('');
  }

  async function seedTicker() {
    try {
      const recent = await ctx.api.events('', 24); // '' = every project
      if (Array.isArray(recent) && recent.length) {
        ticks = recent.slice().sort((a, b) => (Number(b.ts) || 0) - (Number(a.ts) || 0)).slice(0, 24);
      }
    } catch (_) { /* keep */ }
    renderTicker();
  }

  function skeleton() {
    kpisBox.innerHTML = '<div class="mc-kpi"><span class="mck-num skel" style="width:48px;height:30px"></span></div>'.repeat(5);
    pulseWrap.innerHTML = '<div class="skel" style="height:84px;border-radius:7px"></div>';
    bento.innerHTML = '<div class="proj-card skel-card"></div>'.repeat(6);
    attnSection.hidden = true;
  }

  ctx.onEvent((evt) => {
    if (!evt || !evt.type) return;
    ticks.unshift(evt);
    if (ticks.length > 40) ticks.length = 40;
    if (!root.hidden) renderTicker();
    if (!root.hidden && evt.project) {
      const el = cardByName.get(evt.project);
      if (el) { el.classList.remove('flash'); void el.offsetWidth; el.classList.add('flash'); }
    }
  });

  return {
    activate() { refresh(); seedTicker(); },
  };
}
