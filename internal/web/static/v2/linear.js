// Linear connector strip — Linear as a first-class state. On the mirror project
// it shows the live SSOT link: team, active cycle, poll cadence and a "synced Xs
// ago" ticker driven by /api/health .linear_connector. When the last reconcile is
// older than 3× the poll interval the strip goes amber ("reconcile en retard").
import { fmtAgo, parseGoDuration } from './api.js';

export function initLinearStrip(el, ctx) {
  const esc = ctx.esc;
  let project = null;
  let connector = null;
  let cycleName = '';
  let intervalMs = 60000;
  let tick = null;       // 1s "synced ago" ticker
  let poll = null;       // 60s health refetch
  let token = 0;

  function stop() {
    if (tick) { clearInterval(tick); tick = null; }
    if (poll) { clearInterval(poll); poll = null; }
  }

  async function fetchState() {
    const t = ++token;
    const [health, cycles] = await Promise.all([
      ctx.api.health().catch(() => null),
      ctx.api.cycles(project).catch(() => []),
    ]);
    if (t !== token) return;
    connector = (health && health.linear_connector) || null;
    const active = Array.isArray(cycles) ? cycles.find((c) => c.active) : null;
    cycleName = active ? active.name : '';
    const iv = ctx.settings.linear && ctx.settings.linear.interval;
    intervalMs = parseGoDuration(iv) || 60000;
    render();
  }

  function render() {
    if (!connector) { el.innerHTML = '<span class="ls-quiet">linear connector unavailable</span>'; return; }
    const team = connector.team_key || (ctx.settings.linear && ctx.settings.linear.team_key) || 'linear';
    const last = Date.parse(connector.last_reconcile_at) || 0;
    const age = last ? Date.now() - last : Infinity;
    const stale = last && age > intervalMs * 3;
    const pollLabel = humanInterval(intervalMs);
    el.dataset.state = stale ? 'stale' : 'synced';
    el.innerHTML = `
      <span class="ls-mirror"><span class="lin-diamond" aria-hidden="true">◆</span>mirror</span>
      <span class="ls-team">${esc(team)}</span>
      ${cycleName ? `<span class="ls-sep">·</span><span class="ls-cycle">${esc(cycleName)}</span>` : ''}
      <span class="ls-sep">·</span><span class="ls-poll">poll ${esc(pollLabel)}</span>
      <span class="ls-sep">·</span>
      ${stale
        ? `<span class="ls-sync stale"><span class="ls-warn" aria-hidden="true">▲</span>reconcile en retard</span>`
        : `<span class="ls-sync"><span class="ls-pip" aria-hidden="true"></span>synced <span class="ls-ago" data-since="${esc(connector.last_reconcile_at)}">${last ? fmtAgo(last) : '—'} ago</span></span>`}
      ${connector.writer_failures ? `<span class="ls-sep">·</span><span class="ls-fail">${connector.writer_failures} write err</span>` : ''}`;
  }

  function refreshAgo() {
    const ago = el.querySelector('.ls-ago');
    if (!ago) return;
    const since = Date.parse(ago.dataset.since) || 0;
    if (!since) return;
    ago.textContent = `${fmtAgo(since)} ago`;
    // Flip to stale live, without refetching.
    if (Date.now() - since > intervalMs * 3 && el.dataset.state !== 'stale') render();
  }

  function humanInterval(ms) {
    if (ms >= 3600e3) return `${Math.round(ms / 3600e3)}h`;
    if (ms >= 60e3) return `${Math.round(ms / 60e3)}m`;
    return `${Math.round(ms / 1e3)}s`;
  }

  // Refetch health when Linear-side activity lands (cheap, throttled by interval).
  ctx.onEvent((evt) => {
    if (!project || el.hidden || !connector) return;
    if (String(evt.type || '').startsWith('task')) scheduleRefetch();
  });
  let refetchT = null;
  function scheduleRefetch() {
    if (refetchT) return;
    refetchT = setTimeout(() => { refetchT = null; fetchState(); }, 4000);
  }

  return {
    update(proj) {
      // Mirror project only.
      if (!ctx.isMirror(proj)) { project = null; el.hidden = true; stop(); return; }
      const changed = proj !== project;
      project = proj;
      el.hidden = false;
      if (changed) { connector = null; el.innerHTML = '<span class="ls-quiet skel" style="width:220px;height:11px"></span>'; }
      stop();
      tick = setInterval(refreshAgo, 1000);
      poll = setInterval(fetchState, 60000);
      fetchState();
    },
  };
}
