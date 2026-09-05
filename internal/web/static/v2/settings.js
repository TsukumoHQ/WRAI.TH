// Config panel — the writable non-federation settings the console client asked
// for. Reads/writes /api/settings through the existing server-side allowlist
// (writableSettings, api.go). Federation stays on its own page (federation.js);
// this panel owns sun_type + the Linear connector keys.
//
// Two hard rules mirrored from the backend:
//   - Secret (linear_api_key) is shown MASKED and never rendered in full or
//     logged. Leaving it blank on save keeps the stored secret (we simply don't
//     send the key), same "blank = unchanged" contract federation.js uses.
//   - Only allowlist keys are ever sent. Every field's key comes from FIELDS
//     below (all writableSettings entries), so an out-of-allowlist key can't be
//     assembled here; the server also rejects the whole PUT if one slips in, and
//     that rejection is surfaced to the user, not swallowed.
//
// GET /api/settings returns current values for sun_type + linear
// enabled/api_key_masked/team_key/project/interval. It does NOT return
// linear_routing / linear_project_map, so those two are WRITE-ONLY here (labeled
// as such; blank keeps the stored value) — exposing them read/write would need a
// backend change this slice is scoped to avoid.
import { api } from './api.js';

const FIELDS = [
  { key: 'sun_type', group: 'Console', label: 'Sun variant', type: 'text',
    hint: 'console theme variant (e.g. 1)', get: (s) => s.sun_type || '' },
  { key: 'linear_enabled', group: 'Linear', label: 'Connector enabled', type: 'check', linear: true,
    hint: 'mirror Linear issues into the relay', get: (s) => !!(s.linear && s.linear.enabled) },
  { key: 'linear_api_key', group: 'Linear', label: 'API key', type: 'secret', linear: true,
    hint: 'personal Linear token — stored server-side, shown masked; blank keeps current',
    get: (s) => (s.linear && s.linear.api_key_masked) || '' },
  { key: 'linear_team_key', group: 'Linear', label: 'Team key', type: 'text', linear: true,
    hint: 'e.g. SYN', get: (s) => (s.linear && s.linear.team_key) || '' },
  { key: 'linear_project', group: 'Linear', label: 'Mirror project', type: 'text', linear: true,
    hint: 'relay project hosting the mirror', get: (s) => (s.linear && s.linear.project) || '' },
  { key: 'linear_reconcile_interval', group: 'Linear', label: 'Reconcile interval', type: 'text', linear: true,
    hint: 'e.g. 5m', get: (s) => (s.linear && s.linear.interval) || '' },
  { key: 'linear_routing', group: 'Linear', label: 'Routing map (JSON)', type: 'json', linear: true, writeOnly: true,
    hint: '{ "linearProjectId": "agentName" } — write-only; blank keeps current', get: () => '' },
  { key: 'linear_project_map', group: 'Linear', label: 'Project map (JSON)', type: 'json', linear: true, writeOnly: true,
    hint: '{ "linearProjectId": "relayProject" } — write-only; blank keeps current', get: () => '' },
];

export function initSettings(el, ctx) {
  const esc = ctx.esc;
  let s = null;            // last-loaded settings snapshot
  let msg = null;          // {kind:'ok'|'err', text}
  // Boards group (S7b-2): archive a board through POST /api/boards/{id}/archive.
  // Independent of the settings snapshot above so it works even when the config
  // load fails. A refusal is shown verbatim inline; the row stays, no force, no retry.
  let boards = [];
  let boardsProject = null;

  async function load() {
    try {
      s = await api.settings();
    } catch (e) {
      s = null;
      msg = { kind: 'err', text: `load failed: ${e.message}` };
    }
    render();
  }

  // The Linear connector can be pinned by env (RELAY_LINEAR_*). When it is, the
  // backend ignores DB writes for those keys, so we lock the inputs (federation.js
  // does the same) rather than pretend a save took.
  const linearLocked = () => !!(s && s.linear && s.linear.source === 'env');

  function fieldHTML(f) {
    // A failed load (s === null) disables EVERY input: render() below also
    // replaces Save with a Retry button and wire() binds no save handler, so a
    // click can never assemble a body of blanks/'0' that would wipe the stored
    // config (the data-loss path). Env-pinned Linear stays locked as before.
    const locked = (!s || (f.linear && linearLocked())) ? 'disabled' : '';
    const v = s ? f.get(s) : (f.type === 'check' ? false : '');
    if (f.type === 'check') {
      return `<label class="cfg-row cfg-check">
        <span class="cfg-lbl">${esc(f.label)}<small class="cfg-hint">${esc(f.hint)}</small></span>
        <input type="checkbox" data-key="${f.key}" data-type="check" ${v ? 'checked' : ''} ${locked}>
      </label>`;
    }
    if (f.type === 'json') {
      return `<label class="cfg-row">
        <span class="cfg-lbl">${esc(f.label)}<small class="cfg-hint">${esc(f.hint)}</small></span>
        <textarea class="cfg-in cfg-json" data-key="${f.key}" data-type="json" rows="2" spellcheck="false"
          placeholder="write-only — leave blank to keep" ${locked}></textarea>
      </label>`;
    }
    if (f.type === 'secret') {
      // Never put the secret in a value= attribute. Masked hint only; the real
      // input starts empty and is only sent when the user types a new key.
      const mask = v ? `${esc(v)} (unchanged)` : 'not set';
      return `<label class="cfg-row">
        <span class="cfg-lbl">${esc(f.label)}<small class="cfg-hint">${esc(f.hint)}</small></span>
        <input type="password" class="cfg-in" data-key="${f.key}" data-type="secret" autocomplete="off"
          value="" placeholder="${mask}" ${locked}>
      </label>`;
    }
    return `<label class="cfg-row">
      <span class="cfg-lbl">${esc(f.label)}<small class="cfg-hint">${esc(f.hint)}</small></span>
      <input type="text" class="cfg-in" data-key="${f.key}" data-type="text" value="${esc(v)}" ${locked}>
    </label>`;
  }

  function groupHTML(name) {
    const rows = FIELDS.filter((f) => f.group === name).map(fieldHTML).join('');
    const locked = name === 'Linear' && linearLocked();
    const note = locked
      ? `<div class="cfg-note cfg-note-warn">Linear is configured via environment (<code>RELAY_LINEAR_*</code>). Read-only here — unset the env vars to manage it from the console.</div>`
      : '';
    return `<section class="cfg-group">
      <h3 class="cfg-gtitle">${esc(name)}</h3>${note}
      <div class="cfg-fields">${rows}</div>
    </section>`;
  }

  // The Boards group: a project picker + one row per active board with an
  // Archive button and an inline slot for a verbatim server refusal. No force
  // control is rendered and no retry is performed — the row is removed only when
  // the server returns 200 (loadBoards refetches the active set).
  function boardsGroupHTML() {
    const projs = (ctx.projects || []).map((p) => p.name);
    if (boardsProject == null) {
      boardsProject = (ctx.selection && ctx.selection !== 'all') ? ctx.selection : (projs[0] || '');
    }
    const opts = projs.map((n) => `<option value="${esc(n)}" ${n === boardsProject ? 'selected' : ''}>${esc(n)}</option>`).join('');
    const rows = boards.length
      ? boards.map((b) => `
          <div class="cfg-row" data-board="${esc(b.id)}">
            <span class="cfg-lbl">${esc(b.name || b.slug || b.id)}</span>
            <button class="cfg-save board-arch" data-board="${esc(b.id)}">Archive</button>
          </div>
          <div class="board-msg" data-board="${esc(b.id)}" role="status"></div>`).join('')
      : `<div class="cfg-note">${boardsProject ? 'No active boards.' : 'No project selected.'}</div>`;
    return `<section class="cfg-group">
      <h3 class="cfg-gtitle">Boards</h3>
      <div class="cfg-note">Archive a board. Refused if it still holds open Linear-mirrored tasks.</div>
      <label class="cfg-row"><span class="cfg-lbl">Project</span>
        <select class="cfg-in" id="boardsProj" ${projs.length ? '' : 'disabled'}>${opts}</select></label>
      <div class="cfg-fields">${rows}</div>
    </section>`;
  }

  async function loadBoards() {
    try {
      boards = boardsProject ? (await api.boards(boardsProject)) : [];
    } catch (_) {
      boards = [];
    }
    if (!Array.isArray(boards)) boards = [];
    render();
  }

  // Best-effort "which ones": list the open Linear-mirrored tasks still on the
  // refused board, from the existing /api/tasks/all data (no new endpoint).
  async function showOffenders(id, msgEl) {
    if (!msgEl) return;
    let tasks = [];
    try { tasks = await api.tasksAll(); } catch (_) { return; }
    const open = (tasks || []).filter((t) => t.board_id === id && t.source === 'linear'
      && t.archived_at == null && t.status !== 'done' && t.status !== 'cancelled');
    if (!open.length) return;
    const ul = document.createElement('ul');
    ul.className = 'board-offenders';
    ul.innerHTML = open.slice(0, 20).map((t) => `<li>${esc(t.title || t.id)}</li>`).join('');
    msgEl.appendChild(ul);
  }

  function wireBoards() {
    const sel = el.querySelector('#boardsProj');
    if (sel) sel.onchange = () => { boardsProject = sel.value; loadBoards(); };
    el.querySelectorAll('.board-arch').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.board;
        const msgEl = el.querySelector(`.board-msg[data-board="${id}"]`);
        if (msgEl) { msgEl.textContent = ''; msgEl.className = 'board-msg'; }
        btn.disabled = true;
        try {
          await api.archiveBoard(boardsProject, id);
          await loadBoards();                 // 200 → row gone from the active set
        } catch (e) {
          // Verbatim server refusal. Keep the row and the button (a human may act
          // and click again); no automatic retry, no force/override path.
          if (msgEl) { msgEl.textContent = e.message; msgEl.className = 'cfg-note cfg-note-err'; }
          btn.disabled = false;
          await showOffenders(id, msgEl);
        }
      };
    });
  }

  function render() {
    const banner = msg
      ? `<div class="cfg-note ${msg.kind === 'err' ? 'cfg-note-err' : 'cfg-note-ok'}" role="status">${esc(msg.text)}</div>`
      : '';
    el.innerHTML = `
      <section class="cfg-wrap">
        <header class="cfg-head">
          <h2>Configuration</h2>
          <p class="cfg-sub">Writable relay settings. Federation peers live on their own page.</p>
        </header>
        ${banner}
        ${groupHTML('Console')}
        ${groupHTML('Linear')}
        ${boardsGroupHTML()}
        <div class="cfg-actions">
          ${s
            ? `<button class="cfg-save" id="cfgSave">Save</button>`
            : `<button class="cfg-save" id="cfgRetry">Retry load</button>`}
        </div>
      </section>`;
    wire();
  }

  // Build the PUT body from the current field values. Keys come only from FIELDS
  // (all allowlist entries). Secrets/JSON are omitted when blank ("keep current").
  // Returns { body } or { error } if a JSON field is malformed.
  function collect() {
    const body = {};
    for (const f of FIELDS) {
      if (f.linear && linearLocked()) continue;         // env-pinned → never write
      const node = el.querySelector(`[data-key="${f.key}"]`);
      if (!node) continue;
      if (f.type === 'check') { body[f.key] = node.checked ? '1' : '0'; continue; }
      const raw = (node.value || '').trim();
      if (f.type === 'secret') { if (raw) body[f.key] = raw; continue; }   // blank = keep
      if (f.type === 'json') {
        if (!raw) continue;                             // blank = keep
        try { JSON.parse(raw); } catch (_) { return { error: `${f.label}: not valid JSON` }; }
        body[f.key] = raw; continue;
      }
      body[f.key] = raw;                                // plain text
    }
    return { body };
  }

  function wire() {
    // The Boards group is independent of the settings snapshot — wire it first so
    // it stays live even when the config load failed (s === null).
    wireBoards();
    // Failed load: no settings snapshot, so bind ONLY the Retry button and
    // return before any save handler exists — collect()/PUT is unreachable, so a
    // click cannot wipe the stored config.
    if (!s) {
      const retry = el.querySelector('#cfgRetry');
      if (retry) retry.onclick = () => { msg = null; load(); };
      return;
    }
    const save = el.querySelector('#cfgSave');
    if (!save) return;
    save.onclick = async () => {
      const { body, error } = collect();
      if (error) { msg = { kind: 'err', text: error }; return render(); }
      save.disabled = true;
      try {
        await api.saveSettings(body);
        msg = { kind: 'ok', text: 'saved' };
        await load();                                   // reload → reflect persisted values
      } catch (e) {
        // Surface the server's reason (e.g. a key the allowlist refused), don't swallow.
        msg = { kind: 'err', text: `save failed: ${e.message}` };
        render();
      }
    };
  }

  return { activate() { msg = null; load(); loadBoards(); } };
}
