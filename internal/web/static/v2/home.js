// Project home — the landing. Every project is a place rendered as a card in a
// bento grid; clicking a card ENTERS that project. The all-projects aggregate
// lives here and only here (the old global dropdown is gone). The mirror project
// wears a Linear badge with a click-through ↗.
export function initHome(root, ctx) {
  const esc = ctx.esc;
  const bento = root.querySelector('#bento');
  const agg = root.querySelector('#homeAgg');
  let projects = [];
  let loaded = false;
  const cardByName = new Map();

  const fmtNum = (n) => {
    n = Number(n) || 0;
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  };

  async function refresh() {
    if (!loaded) skeleton();
    let list = [];
    try { list = await ctx.api.projects(); } catch (_) { list = ctx.projects || []; }
    projects = Array.isArray(list) ? list : [];
    loaded = true;
    renderAgg();
    renderBento();
  }

  function renderAgg() {
    const agents = projects.reduce((a, p) => a + (Number(p.agent_count) || 0), 0);
    const online = projects.reduce((a, p) => a + (Number(p.online_count) || 0), 0);
    const total = projects.reduce((a, p) => a + (Number(p.total_tasks) || 0), 0);
    const done = projects.reduce((a, p) => a + (Number(p.done_tasks) || 0), 0);
    const active = projects.reduce((a, p) => a + (Number(p.active_tasks) || 0), 0);
    const pct = total ? Math.round(done / total * 100) : 0;
    agg.innerHTML = [
      aggStat(String(projects.length), 'projects'),
      aggStat(`${fmtNum(online)}<span class="of">/${fmtNum(agents)}</span>`, 'agents online'),
      aggStat(String(active), 'active tasks'),
      aggStat(`${pct}%`, `${fmtNum(done)}/${fmtNum(total)} done`),
    ].join('');
  }
  const aggStat = (num, label) =>
    `<div class="agg-stat"><span class="agg-num">${num}</span><span class="agg-label">${esc(label)}</span></div>`;

  function renderBento() {
    if (!projects.length) {
      bento.innerHTML = '<div class="empty">No projects yet.</div>';
      return;
    }
    cardByName.clear();
    // Mirror project first and featured; then by activity (online, then active, then tasks).
    const sorted = projects.slice().sort((a, b) => {
      const am = ctx.isMirror(a.name) ? 1 : 0, bm = ctx.isMirror(b.name) ? 1 : 0;
      if (am !== bm) return bm - am;
      return (b.online_count - a.online_count) || (b.active_tasks - a.active_tasks) ||
        (b.total_tasks - a.total_tasks) || a.name.localeCompare(b.name);
    });
    bento.innerHTML = sorted.map(card).join('');
    bento.querySelectorAll('.proj-card').forEach((el) => {
      cardByName.set(el.dataset.name, el);
      const enter = () => { location.hash = el.dataset.href; };
      el.addEventListener('click', enter);
      el.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); enter(); } });
    });
    // The inner ↗ link opens Linear without entering the project.
    bento.querySelectorAll('.pc-linear').forEach((a) => a.addEventListener('click', (e) => e.stopPropagation()));
  }

  function card(p) {
    const mirror = ctx.isMirror(p.name);
    const total = Number(p.total_tasks) || 0;
    const done = Number(p.done_tasks) || 0;
    const active = Number(p.active_tasks) || 0;
    const online = Number(p.online_count) || 0;
    const agents = Number(p.agent_count) || 0;
    const pct = total ? Math.round(done / total * 100) : 0;
    const live = online > 0 || active > 0;
    const tokens = Number(p.tokens_24h) || 0;
    return `<div class="proj-card${mirror ? ' is-mirror' : ''}${live ? ' is-live' : ''}" role="link" tabindex="0" data-name="${esc(p.name)}" data-href="#/p/${encodeURIComponent(p.name)}/overview" aria-label="Enter project ${esc(p.name)}">
      <div class="pc-top">
        <span class="pc-name">${esc(p.name)}</span>
        ${mirror ? `<a class="pc-linear" href="${esc(ctx.linearTeamURL())}" target="_blank" rel="noopener" title="Mirrored from Linear — open Linear"><span class="lin-diamond" aria-hidden="true">◆</span>${esc((ctx.settings.linear && ctx.settings.linear.team_key) || 'linear')}<span class="lin-arrow" aria-hidden="true">↗</span></a>` : ''}
      </div>
      <div class="pc-agents">
        <span class="pc-dot${live ? ' on' : ''}" aria-hidden="true"></span>
        <span class="pc-agents-n">${online ? `${online} online` : `${agents} agent${agents === 1 ? '' : 's'}`}</span>
        ${online && agents !== online ? `<span class="pc-agents-sub">· ${agents} total</span>` : ''}
      </div>
      <div class="pc-bar" aria-hidden="true"><i style="width:${pct}%"></i></div>
      <div class="pc-foot">
        <span class="pc-foot-tasks">${done}<span class="of">/${total}</span> done</span>
        <span class="pc-foot-meta">${active ? `${active} active` : (tokens ? `${fmtNum(tokens)} tok` : 'idle')}</span>
      </div>
    </div>`;
  }

  function skeleton() {
    agg.innerHTML = '<div class="agg-stat"><span class="agg-num skel" style="width:42px;height:24px"></span></div>'.repeat(4);
    bento.innerHTML = '<div class="proj-card skel-card"></div>'.repeat(6);
  }

  // Live: flash the card of a project that just saw activity.
  ctx.onEvent((evt) => {
    if (root.hidden || !evt || !evt.project) return;
    const el = cardByName.get(evt.project);
    if (!el) return;
    el.classList.remove('flash');
    void el.offsetWidth;
    el.classList.add('flash');
  });

  return {
    activate() { refresh(); },
  };
}
