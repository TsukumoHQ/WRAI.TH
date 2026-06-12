// Stats — hand-rolled SVG charts over /api/stats. Every chart has <title>
// tooltips and a table fallback toggle. Durations are seconds from the API.
import { fmtDur, colorFor, initialFor, prefersReducedMotion } from './api.js';

export function initStats(root, ctx) {
  const esc = ctx.esc;
  const cycleEl = root.querySelector('#statsCycleFilter');
  const kpiEl = root.querySelector('#statKpis');
  let cycle = 'active';
  let data = null;
  let loadedFor = null;
  let token = 0;

  const sel = () => ctx.selection;

  async function load(resetCycle = true) {
    const project = sel();
    if (project === 'all') {
      cycleEl.hidden = true;
      kpiEl.innerHTML = '';
      setAll('<div class="empty">Select a single project to view its stats.</div>');
      loadedFor = 'all';
      return;
    }
    const t = ++token;
    skeleton();
    let d = null;
    try { d = await ctx.api.stats(project, resetCycle ? undefined : cycle); } catch (_) { d = null; }
    if (t !== token) return;
    data = d;
    if (resetCycle) cycle = (d && d.cycle && d.cycle.id) || 'active';
    loadedFor = `${project}|${cycle}`;
    renderCycleFilter();
    render();
  }

  function renderCycleFilter() {
    const cycles = (data && data.cycles) || [];
    if (!cycles.length) { cycleEl.hidden = true; cycleEl.innerHTML = ''; return; }
    cycleEl.hidden = false;
    const pills = cycles.map((c) =>
      `<button class="cyc-pill${cycle === c.id ? ' on' : ''}" data-cycle="${esc(c.id)}">${esc(c.name)}${c.active ? ' ●' : ''}</button>`);
    pills.push(`<button class="cyc-pill${cycle === 'all' ? ' on' : ''}" data-cycle="all">all time</button>`);
    cycleEl.innerHTML = pills.join('');
    cycleEl.querySelectorAll('.cyc-pill').forEach((b) => b.addEventListener('click', () => { cycle = b.dataset.cycle; load(false); }));
  }

  /* ---------------- KPI tiles ---------------- */
  function render() {
    if (!data || data.empty) {
      kpiEl.innerHTML = '';
      setAll('<div class="empty">No data in this scope yet.</div>');
      return;
    }
    const ct = data.cycle_time?.overall?.claim_to_done || {};
    const tis = data.time_in_state || {};
    const bottleneck = { todo: 'Todo', in_progress: 'In Progress', in_review: 'In Review' }[tis.bottleneck] || '—';
    const blockedTotal = data.blocked?.total_seconds || 0;
    kpiEl.innerHTML = [
      tile('median cycle time', fmtDur(ct.median), `p90 ${fmtDur(ct.p90)} · n=${ct.count || 0}`),
      tile('throughput', String(data.throughput?.tasks_done || 0), `${data.throughput?.points_done || 0} pts done`),
      tile('blocked', fmtDur(blockedTotal), `${data.blocked?.episode_count || 0} episodes`),
      tile('bottleneck', bottleneck, bottleneck === '—' ? 'balanced' : `slowest state`),
    ].join('');
    renderThroughput();
    renderBurndown();
    renderTimeState();
    renderPerAgent();
    renderBlocked();
  }
  const tile = (label, num, sub) =>
    `<article class="card kpi"><span class="kpi-label">${label}</span><span class="kpi-num">${esc(num)}</span><span class="kpi-sub">${esc(sub)}</span></article>`;

  /* ---------------- chart card scaffold + table toggle ---------------- */
  function chartCard(id, label, meta, chartHtml, tableHtml) {
    const el = root.querySelector('#' + id);
    el.innerHTML = `<div class="card-head"><span class="card-label">${label}</span>
      <span class="card-meta">${meta || ''} <button class="tbl-toggle" type="button" aria-pressed="false">table</button></span></div>
      <div class="chart-wrap">${chartHtml}</div>
      <div class="chart-table" hidden>${tableHtml || ''}</div>`;
    const btn = el.querySelector('.tbl-toggle');
    btn.addEventListener('click', () => {
      const tbl = el.querySelector('.chart-table'), wrap = el.querySelector('.chart-wrap');
      const show = tbl.hidden;
      tbl.hidden = !show; wrap.hidden = show;
      btn.setAttribute('aria-pressed', String(show));
      btn.textContent = show ? 'chart' : 'table';
    });
  }

  /* ---------------- line chart (cumulative / burndown) ---------------- */
  // series: [{x:label, ys:[{v,color,dash?,label}]}]  shared axis
  function lineChart(points, lines, fmtY = (v) => v) {
    const W = 560, H = 180, pl = 38, pr = 12, pt = 12, pb = 22;
    const n = points.length;
    if (!n) return '<div class="empty">No series data</div>';
    const allV = lines.flatMap((l) => l.values);
    const maxV = Math.max(1, ...allV), minV = Math.min(0, ...allV);
    const span = maxV - minV || 1;
    const x = (i) => pl + (n === 1 ? 0 : i / (n - 1) * (W - pl - pr));
    const y = (v) => pt + (1 - (v - minV) / span) * (H - pt - pb);
    const grid = [0, 0.5, 1].map((f) => {
      const gv = minV + f * span, gy = y(gv);
      return `<line x1="${pl}" y1="${gy.toFixed(1)}" x2="${W - pr}" y2="${gy.toFixed(1)}" class="grid"/><text x="2" y="${(gy + 3).toFixed(1)}" class="ax">${esc(fmtY(Math.round(gv)))}</text>`;
    }).join('');
    const paths = lines.map((l) => {
      const d = l.values.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(' ');
      const dots = l.dash ? '' : l.values.map((v, i) => `<circle cx="${x(i).toFixed(1)}" cy="${y(v).toFixed(1)}" r="2.4" fill="${l.color}"><title>${esc(points[i])}: ${esc(fmtY(v))}</title></circle>`).join('');
      const area = l.fill ? `<path d="${d} L${x(n - 1).toFixed(1)} ${y(minV).toFixed(1)} L${x(0).toFixed(1)} ${y(minV).toFixed(1)} Z" fill="${l.color}" opacity="0.08"/>` : '';
      return `${area}<path d="${d}" fill="none" stroke="${l.color}" stroke-width="1.8"${l.dash ? ' stroke-dasharray="4 4" opacity="0.7"' : ''} class="${prefersReducedMotion() ? '' : 'draw'}"/>${dots}`;
    }).join('');
    const xticks = [0, Math.floor(n / 2), n - 1].filter((v, i, a) => a.indexOf(v) === i)
      .map((i) => `<text x="${x(i).toFixed(1)}" y="${H - 6}" class="ax mid">${esc(points[i])}</text>`).join('');
    const legend = lines.filter((l) => l.label).map((l) => `<span><i class="ld" style="background:${l.color}${l.dash ? ';opacity:.6' : ''}"></i>${esc(l.label)}</span>`).join('');
    return `<svg class="chart-svg" viewBox="0 0 ${W} ${H}" preserveAspectRatio="xMidYMid meet" role="img">${grid}${paths}${xticks}</svg>${legend ? `<div class="legend chart-legend">${legend}</div>` : ''}`;
  }

  function renderThroughput() {
    const s = data.throughput?.series || [];
    const chart = lineChart(
      s.map((p) => p.date.slice(5)),
      [{ values: s.map((p) => p.cumulative_tasks || 0), color: 'var(--accent)', fill: true, label: 'tasks' }],
    );
    const table = simpleTable(['date', 'tasks', 'points'], s.map((p) => [p.date, p.cumulative_tasks, p.cumulative_points]));
    chartCard('cardThroughput', 'throughput · cumulative', `${data.throughput?.tasks_done || 0} done`, chart, table);
  }

  function renderBurndown() {
    const s = data.burndown?.series || [];
    const chart = lineChart(
      s.map((p) => p.date.slice(5)),
      [
        { values: s.map((p) => p.remaining_tasks || 0), color: 'var(--blue)', label: 'remaining' },
        { values: s.map((p) => p.ideal || 0), color: 'var(--text-muted)', dash: true, label: 'ideal' },
      ],
    );
    const table = simpleTable(['date', 'remaining', 'ideal'], s.map((p) => [p.date, p.remaining_tasks, p.ideal]));
    chartCard('cardBurndown', 'burndown', `${data.burndown?.remaining_tasks || 0} left`, chart, table);
  }

  /* ---------------- time-in-state stacked bar ---------------- */
  function renderTimeState() {
    const tis = data.time_in_state || {};
    const segs = [
      ['Todo', tis.todo?.median || 0, 'var(--slate)'],
      ['In Progress', tis.in_progress?.median || 0, 'var(--blue)'],
      ['In Review', tis.in_review?.median || 0, 'var(--amber)'],
    ];
    const total = segs.reduce((a, [, v]) => a + v, 0);
    let chart;
    if (!total) chart = '<div class="empty">No timing data</div>';
    else chart = `<div class="stack-bar">${segs.filter(([, v]) => v).map(([k, v, c]) =>
      `<i style="width:${(v / total * 100).toFixed(1)}%;background:${c}" title="${k}: ${fmtDur(v)}"></i>`).join('')}</div>
      <div class="legend">${segs.map(([k, v, c]) => `<span><i class="ld" style="background:${c}"></i>${k} <b>${fmtDur(v)}</b></span>`).join('')}</div>`;
    const table = simpleTable(['state', 'median', 'p90'], segs.map(([k, v], i) => {
      const o = [tis.todo, tis.in_progress, tis.in_review][i] || {};
      return [k, fmtDur(v), fmtDur(o.p90)];
    }));
    chartCard('cardTimeState', 'median time in state', tis.bottleneck ? `bottleneck: ${tis.bottleneck.replace('_', ' ')}` : '', chart, table);
  }

  /* ---------------- per-agent horizontal bars ---------------- */
  function renderPerAgent() {
    const rows = (data.throughput?.per_agent || []).slice().sort((a, b) => b.tasks_done - a.tasks_done);
    const max = Math.max(1, ...rows.map((r) => r.tasks_done));
    let chart;
    if (!rows.length) chart = '<div class="empty">No agent throughput</div>';
    else chart = `<div class="bars">${rows.slice(0, 12).map((r) => `
      <div class="bar-row">
        <span class="avatar sm" style="background:${colorFor(r.agent)}">${esc(initialFor(r.agent))}</span>
        <span class="bar-name" title="${esc(r.agent)}">${esc(r.agent)}</span>
        <span class="bar-track"><i style="width:${(r.tasks_done / max * 100).toFixed(1)}%"></i></span>
        <span class="bar-val">${r.tasks_done}${r.points_done ? ` · ${r.points_done}pt` : ''}</span>
      </div>`).join('')}</div>`;
    const table = simpleTable(['agent', 'tasks', 'points'], rows.map((r) => [r.agent, r.tasks_done, r.points_done]));
    chartCard('cardPerAgent', 'throughput by agent', `${rows.length} agents`, chart, table);
  }

  /* ---------------- currently blocked ---------------- */
  function renderBlocked() {
    const el = root.querySelector('#cardBlocked');
    const list = data.blocked?.currently_blocked || [];
    el.innerHTML = `<div class="card-head"><span class="card-label">currently blocked</span><span class="card-meta">${list.length} task${list.length === 1 ? '' : 's'}</span></div>`;
    if (!list.length) { el.innerHTML += '<div class="empty">Nothing blocked right now ✓</div>'; return; }
    el.innerHTML += '<div class="blocked-list">' + list.map((b) => `
      <div class="blocked-row">
        <span class="blk-dot"></span>
        ${b.linear_key ? `<span class="chip-key">${esc(b.linear_key)}</span>` : ''}
        <span class="blocked-title" title="${esc(b.title)}">${esc(b.title)}</span>
        <span class="avatar sm" style="background:${colorFor(b.agent)}">${esc(initialFor(b.agent))}</span>
        <span class="blocked-since">${fmtDur(b.since_seconds)}</span>
      </div>`).join('') + '</div>';
  }

  /* ---------------- helpers ---------------- */
  function simpleTable(headers, rows) {
    if (!rows.length) return '<div class="empty">No rows</div>';
    return `<table class="dtable"><thead><tr>${headers.map((h) => `<th>${esc(h)}</th>`).join('')}</tr></thead>
      <tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${esc(c == null ? '' : c)}</td>`).join('')}</tr>`).join('')}</tbody></table>`;
  }
  function setAll(html) {
    ['cardThroughput', 'cardBurndown', 'cardTimeState', 'cardPerAgent', 'cardBlocked']
      .forEach((id) => { root.querySelector('#' + id).innerHTML = html; });
  }
  function skeleton() {
    kpiEl.innerHTML = '<article class="card kpi" data-skel><span class="kpi-label">loading</span><span class="kpi-num">—</span></article>'.repeat(4);
    setAll('<div class="skel" style="height:120px"></div>');
  }

  ctx.onSelection(() => { if (!root.hidden) load(true); else loadedFor = null; });

  return {
    activate() { if (loadedFor !== `${sel()}|${cycle}`) load(true); },
  };
}
