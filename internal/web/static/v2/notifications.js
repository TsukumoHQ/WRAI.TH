// Notifications — rule rows (event / action / target) with toggle switches, an
// add-rule row, inline test-fire payloads, and the delivery log.
import { fmtAgo } from './api.js';

const EVENTS = [
  'task.dispatched', 'task.claimed', 'task.in_progress', 'task.blocked',
  'task.in_review', 'task.done', 'cycle.digest',
];
const ACTIONS = ['message', 'webhook', 'slack'];

export function initNotifications(root, ctx) {
  const esc = ctx.esc;
  const rulesBody = root.querySelector('#rulesBody');
  const rulesMeta = root.querySelector('#rulesMeta');
  const delBody = root.querySelector('#deliveriesBody');
  const delMeta = root.querySelector('#deliveriesMeta');
  let rules = [];
  let loadedFor = null;

  const sel = () => ctx.selection;

  async function load() {
    const project = sel();
    rulesBody.innerHTML = '<div class="skel" style="height:48px"></div>'.repeat(3);
    delBody.innerHTML = '<div class="skel" style="height:32px"></div>'.repeat(4);
    if (project === 'all') {
      rules = [];
      rulesMeta.textContent = '';
      rulesBody.innerHTML = '<div class="empty">Select a single project to manage its rules.</div>';
    } else {
      try { rules = await ctx.api.notificationRules(project); } catch (_) { rules = []; }
      if (!Array.isArray(rules)) rules = [];
      renderRules();
    }
    loadDeliveries();
    loadedFor = project;
  }

  /* ---------------- rules ---------------- */
  function renderRules() {
    rulesMeta.textContent = `${rules.length} rule${rules.length === 1 ? '' : 's'}`;
    const rows = rules.map(ruleRow).join('');
    rulesBody.innerHTML = `<div class="rule-cols"><span>event</span><span>action</span><span>target</span><span></span></div>${rows || '<div class="empty">No rules yet — add one below.</div>'}${addRow()}`;
    bindRules();
  }

  function ruleRow(r) {
    return `<div class="rule-row" data-id="${esc(r.id)}">
      <button class="switch${r.enabled ? ' on' : ''}" data-toggle aria-pressed="${!!r.enabled}" aria-label="Toggle rule"><span class="knob"></span></button>
      <div class="rule-main">
        <span class="rule-name">${esc(r.name || '(unnamed)')}</span>
        <div class="rule-fields">
          <span class="rule-event">${esc(r.event)}</span>
          <span class="rule-arrow">→</span>
          <span class="rule-action act-${esc(r.action)}">${esc(r.action)}</span>
          <span class="rule-target">${r.target ? esc(r.target) : '<i class="text-dim">—</i>'}</span>
        </div>
      </div>
      <div class="rule-actions">
        <button class="mini" data-test>test</button>
        <button class="mini danger" data-del aria-label="Delete rule">✕</button>
      </div>
      <div class="rule-payload" hidden></div>
    </div>`;
  }

  function addRow() {
    const evs = EVENTS.map((e) => `<option value="${e}">${e}</option>`).join('') + '<option value="__custom">event:custom…</option>';
    const acts = ACTIONS.map((a) => `<option value="${a}">${a}</option>`).join('');
    return `<form class="rule-add" data-add>
      <input class="ra-name" placeholder="rule name" aria-label="Rule name" required />
      <select class="ra-event" aria-label="Event">${evs}</select>
      <input class="ra-custom" placeholder="custom event" hidden aria-label="Custom event" />
      <select class="ra-action" aria-label="Action">${acts}</select>
      <input class="ra-target" placeholder="target (agent / role / human / url)" aria-label="Target" />
      <button class="qa-btn" type="submit">add rule</button>
    </form>`;
  }

  function bindRules() {
    rulesBody.querySelectorAll('.rule-row').forEach((row) => {
      const id = row.dataset.id;
      row.querySelector('[data-toggle]').addEventListener('click', () => toggleRule(id, row));
      row.querySelector('[data-test]').addEventListener('click', () => testRule(id, row));
      row.querySelector('[data-del]').addEventListener('click', () => deleteRule(id));
    });
    const form = rulesBody.querySelector('[data-add]');
    if (form) {
      const evSel = form.querySelector('.ra-event'), custom = form.querySelector('.ra-custom');
      evSel.addEventListener('change', () => { custom.hidden = evSel.value !== '__custom'; });
      form.addEventListener('submit', addRule);
    }
  }

  async function toggleRule(id, row) {
    const r = rules.find((x) => x.id === id);
    if (!r) return;
    const next = !r.enabled;
    const sw = row.querySelector('.switch');
    sw.classList.toggle('on', next); sw.setAttribute('aria-pressed', String(next));
    try { await ctx.api.patchRule(id, { enabled: next }); r.enabled = next; }
    catch (_) { sw.classList.toggle('on', r.enabled); sw.setAttribute('aria-pressed', String(r.enabled)); }
  }

  async function testRule(id, row) {
    const box = row.querySelector('.rule-payload');
    box.hidden = false;
    box.innerHTML = '<div class="skel" style="height:40px"></div>';
    try {
      const res = await ctx.api.testFireRule(id, false);
      const outcome = res.outcome || (res.sent ? 'ok' : 'dryrun');
      box.innerHTML = `<div class="payload-head"><span class="odot ${outcome}"></span>${esc(outcome)} · dry-run</div><pre class="payload-pre">${esc(JSON.stringify(res.payload || {}, null, 2))}</pre>`;
    } catch (err) {
      box.innerHTML = `<div class="payload-head"><span class="odot failed"></span>${esc(err.message || 'failed')}</div>`;
    }
  }

  async function deleteRule(id) {
    const r = rules.find((x) => x.id === id);
    if (!confirm(`Delete rule "${r ? r.name : id}"?`)) return;
    try { await ctx.api.deleteRule(id); rules = rules.filter((x) => x.id !== id); renderRules(); }
    catch (_) { /* keep */ }
  }

  async function addRule(e) {
    e.preventDefault();
    const form = e.currentTarget;
    const evSel = form.querySelector('.ra-event');
    let event = evSel.value;
    if (event === '__custom') {
      const c = form.querySelector('.ra-custom').value.trim();
      if (!c) return;
      event = c.startsWith('event:') ? c : `event:${c}`;
    }
    const body = {
      project: sel(),
      name: form.querySelector('.ra-name').value.trim(),
      event,
      action: form.querySelector('.ra-action').value,
      target: form.querySelector('.ra-target').value.trim(),
      enabled: true,
    };
    if (!body.name) return;
    const btn = form.querySelector('.qa-btn');
    btn.disabled = true;
    try {
      const created = await ctx.api.createRule(body);
      if (created && created.id) rules.push(created);
      renderRules();
    } catch (err) {
      btn.disabled = false; form.querySelector('.ra-name').classList.add('qa-error');
      setTimeout(() => form.querySelector('.ra-name')?.classList.remove('qa-error'), 1200);
    }
  }

  /* ---------------- deliveries ---------------- */
  async function loadDeliveries() {
    let list = [];
    try { list = await ctx.api.deliveries(50); } catch (_) { list = []; }
    if (!Array.isArray(list)) list = [];
    const project = sel();
    if (project !== 'all') list = list.filter((d) => d.project === project);
    delMeta.textContent = `${list.length} recent`;
    if (!list.length) { delBody.innerHTML = '<div class="empty">No deliveries yet.</div>'; return; }
    delBody.innerHTML = `<table class="dtable deliveries">
      <thead><tr><th></th><th>when</th><th>rule</th><th>event</th><th>action</th><th>target</th></tr></thead>
      <tbody>${list.map((d) => `<tr>
        <td><span class="odot ${esc(d.outcome || 'dryrun')}" title="${esc(d.outcome)}${d.error ? ' · ' + esc(d.error) : ''}"></span></td>
        <td class="text-dim">${fmtAgo(d.created_at)} ago</td>
        <td>${esc(d.rule_name || '—')}</td>
        <td>${esc(d.event)}</td>
        <td class="act-${esc(d.action)}">${esc(d.action)}</td>
        <td class="text-dim">${esc(d.target || '—')}</td>
      </tr>`).join('')}</tbody></table>`;
  }

  ctx.onSelection(() => { if (!root.hidden) load(); else loadedFor = null; });

  return { activate() { if (loadedFor !== sel()) load(); } };
}
