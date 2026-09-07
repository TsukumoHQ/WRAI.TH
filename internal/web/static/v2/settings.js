// Config panel — renders the WHOLE relay configuration surface from the frozen
// GET /api/settings metadata (T2a contract): a `groups` order array + a
// `settings` array where every entry carries {key, group, kind, value, set,
// source, writable, secret, env_name, bounds, default, note}. There is NO
// hardcoded writable key list here anymore; the panel is driven entirely by the
// server metadata, and the server remains the single allowlist authority.
//
// Hard rules mirrored from the backend:
//   - Secrets never render their value (metadata value is always ""); the panel
//     shows a set/unset pill and a never-prefilled 'replace' input. An empty
//     replace input is OMITTED from the PUT (blank = unchanged, never sends "");
//     an explicit 'clear' sends JSON null for that key.
//   - A key is read-only when the server says so (!writable) or when it is
//     resolved from the environment (source === 'env'); locked inputs are
//     disabled so a save can never write them.
//   - Federation is summarised read-only with a link to its own editor page;
//     tokens are never shown here (one editor: v2/federation.js).
//   - No value is ever logged.
import { api } from './api.js';

// Group id (server) -> human header, in the server's `groups` order.
const GROUP_LABELS = {
  console: 'Console',
  linear: 'Linear',
  federation: 'Federation',
  server: 'Server',
  operational: 'Operational',
  timing: 'Timing',
};

// Source badge text (exact, cto Q3). `code` = a compile-time const (Timing).
const SOURCE_BADGE = {
  env: 'env',
  setting: 'setting',
  default: 'default',
  code: 'compile-time (code)',
};

// A key is locked (read-only input) when the server marks it non-writable OR its
// value is resolved from the environment (env wins over any DB write).
const isLocked = (st) => !st.writable || st.source === 'env';

// Human labels + one-line help per key. Keys absent here render their raw key.
// This map is a subset of the server spec keys; every WRITABLE spec key has an
// entry (the panel is friendlier, and the contract test enforces the coverage).
const LABELS = {
  // Console
  sun_type: { label: 'Sun variant', help: 'console theme variant (e.g. 1)' },
  // Linear
  linear_enabled: { label: 'Connector enabled', help: 'mirror Linear issues into the relay' },
  linear_api_key: { label: 'API key', help: 'personal Linear token — stored server-side, never shown' },
  linear_team_key: { label: 'Team key', help: 'e.g. SYN' },
  linear_project: { label: 'Mirror project', help: 'relay project hosting the mirror' },
  linear_reconcile_interval: { label: 'Reconcile interval', help: 'e.g. 5m' },
  linear_routing: { label: 'Routing map (JSON)', help: '{ "linearProjectId": "agentName" }' },
  linear_project_map: { label: 'Project map (JSON)', help: '{ "linearProjectId": "relayProject" }' },
  linear_webhook_secret: { label: 'Webhook secret', help: 'set via RELAY env — read-only here' },
  // Federation
  federation_peers: { label: 'Federation peers', help: 'managed on the federation page' },
  // Operational (writable)
  agent_max_age: { label: 'Agent max age', help: 'idle agent cleanup horizon' },
  message_retention: { label: 'Message retention', help: 'how long messages are kept' },
  audit_log_retention: { label: 'Audit log retention', help: 'how long audit rows are kept' },
  deadletter_short_retention: { label: 'Dead-letter (short)', help: 'short-lived dead-letter retention' },
  deadletter_long_retention: { label: 'Dead-letter (long)', help: 'must be >= the short retention' },
  token_usage_retention_days: { label: 'Token usage retention (days)', help: 'drives purge + rollup window' },
  ack_notify_age: { label: 'Ack notify age', help: 'when an unacked delivery is re-notified' },
  ack_escalate_age: { label: 'Ack escalate age', help: 'must be greater than the notify age' },
  backup_keep: { label: 'Backups kept', help: 'env RELAY_BACKUP_KEEP wins when set' },
  reviewer_ttl_days: { label: 'Reviewer TTL (days)', help: 'env RELAY_REVIEWER_TTL_DAYS wins when set' },
  foreign_backup_min_age: { label: 'Foreign backup min age', help: 'minimum age before a foreign backup is swept' },
  activity_idle_seconds: { label: 'Activity: idle (s)', help: 'seconds before an agent reads as idle' },
  activity_waiting_seconds: { label: 'Activity: waiting (s)', help: 'seconds before an agent reads as waiting' },
  activity_exit_seconds: { label: 'Activity: exit (s)', help: 'seconds before an agent reads as exited' },
  cost_default_model: { label: 'Default cost model', help: 'model id used when a token row has none' },
  // Timing (read-only consts, shown for transparency)
  writer_timeout: { label: 'Writer timeout', help: 'must stay below the referential scan timeout' },
  referential_scan_timeout: { label: 'Referential scan timeout', help: 'writer timeout must stay below this' },
};

export function initSettings(el, ctx) {
  const esc = ctx.esc;
  let s = null;            // last-loaded settings snapshot { groups, settings }
  let msg = null;          // {kind:'ok'|'err', text}
  // Boards group (S7b-2): archive a board through POST /api/boards/{id}/archive.
  // Independent of the settings snapshot above so it works even when the config
  // load fails. A refusal is shown verbatim inline; the row stays, no force, no retry.
  let boards = [];
  let boardsProject = null;

  // A well-formed metadata snapshot has both the groups order and the settings.
  const loaded = () => !!(s && Array.isArray(s.groups) && Array.isArray(s.settings));

  async function load() {
    try {
      s = await api.settings();
      msg = null;
    } catch (e) {
      s = null;
      msg = { kind: 'err', text: `load failed: ${e.message}` };
    }
    render();
  }

  // ---- config rendering (metadata-driven) --------------------------------

  function badgeHTML(st) {
    const text = SOURCE_BADGE[st.source] || st.source || '';
    return `<span class="cfg-badge cfg-badge-${esc(st.source)}">${esc(text)}</span>`;
  }

  function inputHTML(st, locked) {
    const dis = locked ? 'disabled' : '';
    const kind = st.kind;
    if (kind === 'bool') {
      return `<input type="checkbox" data-key="${esc(st.key)}" data-type="bool" ${st.value === '1' ? 'checked' : ''} ${dis}>`;
    }
    if (kind === 'enum') {
      const vals = (st.bounds && Array.isArray(st.bounds.values)) ? st.bounds.values : [];
      const opts = vals.map((v) => `<option value="${esc(v)}" ${v === st.value ? 'selected' : ''}>${esc(v)}</option>`).join('');
      return `<select class="cfg-in" data-key="${esc(st.key)}" data-type="enum" ${dis}>${opts}</select>`;
    }
    if (kind === 'int' || kind === 'duration') {
      const b = st.bounds || {};
      const hint = (b.min != null && b.max != null) ? `${esc(b.min)}..${esc(b.max)}` : '';
      return `<input type="text" class="cfg-in" data-key="${esc(st.key)}" data-type="${kind}" value="${esc(st.value)}" placeholder="${hint}" ${dis}>`;
    }
    if (kind === 'json') {
      return `<textarea class="cfg-in cfg-json" data-key="${esc(st.key)}" data-type="json" rows="2" spellcheck="false" ${dis}>${esc(st.value)}</textarea>`;
    }
    if (kind === 'secret') {
      // Never render the secret value. A pill states presence; the replace input
      // starts empty (no value= attribute) and is only sent when the user types.
      const pill = `<span class="cfg-pill cfg-pill-${st.set ? 'set' : 'unset'}">${st.set ? 'set' : 'unset'}</span>`;
      const replace = locked
        ? ''
        : `<input type="password" class="cfg-in cfg-secret" data-key="${esc(st.key)}" data-type="secret" autocomplete="off" placeholder="replace — leave blank to keep">`;
      const clear = (!locked && st.set)
        ? `<button type="button" class="cfg-clear" data-clear="${esc(st.key)}">clear</button>`
        : '';
      return `<div class="cfg-secret-wrap">${pill}${replace}${clear}</div>`;
    }
    // string (and any unknown kind) → plain text
    return `<input type="text" class="cfg-in" data-key="${esc(st.key)}" data-type="string" value="${esc(st.value)}" ${dis}>`;
  }

  function rowHTML(st) {
    const meta = LABELS[st.key] || null;
    const label = meta ? meta.label : st.key;
    const help = meta ? meta.help : '';
    const locked = isLocked(st);
    const lock = locked ? `<span class="cfg-lock" title="read-only">read-only</span>` : '';
    const note = (st.note && st.note.trim())
      ? `<div class="cfg-note-row">${esc(st.note)}</div>` : '';
    return `<div class="cfg-row cfg-row-meta">
      <span class="cfg-lbl">${esc(label)}
        <small class="cfg-hint">${esc(help)}</small>
        <span class="cfg-tags">${badgeHTML(st)}${lock}</span>
      </span>
      <span class="cfg-ctl">
        ${inputHTML(st, locked)}
        ${note}
        <div class="cfg-err" data-err-key="${esc(st.key)}" role="alert" hidden></div>
      </span>
    </div>`;
  }

  // Federation is summarised read-only (peer count + labels, NEVER tokens) with a
  // link to its dedicated editor page. No inline JSON editor lives here (Q2).
  function federationHTML(settings) {
    const fed = settings.find((st) => st.key === 'federation_peers');
    let count = 0;
    let labels = [];
    if (fed && fed.value) {
      try {
        const arr = JSON.parse(fed.value);
        if (Array.isArray(arr)) {
          count = arr.length;
          labels = arr.map((p) => (p && p.label) ? String(p.label) : '').filter(Boolean);
        }
      } catch (_) { /* malformed → summarise as unknown */ }
    }
    const list = labels.length
      ? `<div class="cfg-fed-labels">${labels.map((l) => `<span class="cfg-fed-peer">${esc(l)}</span>`).join('')}</div>`
      : '';
    return `<section class="cfg-group">
      <h3 class="cfg-gtitle">${esc(GROUP_LABELS.federation)}</h3>
      <div class="cfg-fields">
        <div class="cfg-fed-summary">
          <span class="cfg-fed-count">${count} peer${count === 1 ? '' : 's'} configured</span>
          ${list}
          <a class="cfg-fed-link" href="#/federation">Manage federation peers →</a>
        </div>
      </div>
    </section>`;
  }

  function groupHTML(gid, settings) {
    if (gid === 'federation') return federationHTML(settings);
    const rows = settings.filter((st) => st.group === gid).map(rowHTML).join('');
    if (!rows) return '';
    return `<section class="cfg-group">
      <h3 class="cfg-gtitle">${esc(GROUP_LABELS[gid] || gid)}</h3>
      <div class="cfg-fields">${rows}</div>
    </section>`;
  }

  // ---- Boards group (unchanged from S7b) ----------------------------------

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
          if (msgEl) { msgEl.textContent = e.message; msgEl.className = 'cfg-note cfg-note-err'; }
          btn.disabled = false;
          await showOffenders(id, msgEl);
        }
      };
    });
  }

  // ---- render + save ------------------------------------------------------

  function render() {
    const banner = msg
      ? `<div class="cfg-note ${msg.kind === 'err' ? 'cfg-note-err' : 'cfg-note-ok'}" role="status">${esc(msg.text)}</div>`
      : '';
    const groups = loaded()
      ? s.groups.map((gid) => groupHTML(gid, s.settings)).join('')
      : '';
    el.innerHTML = `
      <section class="cfg-wrap">
        <header class="cfg-head">
          <h2>Configuration</h2>
          <p class="cfg-sub">The full relay configuration surface. Read-only rows are pinned by env or compiled in.</p>
        </header>
        ${banner}
        ${groups}
        ${boardsGroupHTML()}
        <div class="cfg-actions">
          ${loaded()
            ? `<button class="cfg-save" id="cfgSave">Save</button>`
            : `<button class="cfg-save" id="cfgRetry">Retry load</button>`}
        </div>
      </section>`;
    wire();
  }

  // Build the PUT body from the current writable fields. Federation is skipped
  // (its own page), locked rows are skipped (env/non-writable), a blank secret
  // replace is OMITTED (blank = unchanged, never sends ""). Returns { body } or
  // { error } if a JSON field is malformed.
  function collect() {
    const body = {};
    for (const st of s.settings) {
      if (st.group === 'federation') continue;    // summarised, not edited here
      if (isLocked(st)) continue;                 // env-pinned / non-writable
      const node = el.querySelector(`[data-key="${st.key}"]`);
      if (!node) continue;
      if (st.kind === 'bool') { body[st.key] = node.checked ? '1' : '0'; continue; }
      const raw = (node.value || '').trim();
      if (st.kind === 'secret') { if (raw) body[st.key] = raw; continue; }  // blank = keep
      if (st.kind === 'json') {
        if (!raw) { body[st.key] = ''; continue; }
        try { JSON.parse(raw); } catch (_) { return { error: { key: st.key, detail: 'not valid JSON' } }; }
        body[st.key] = raw; continue;
      }
      body[st.key] = raw;                          // string / int / duration / enum
    }
    return { body };
  }

  // Whole-request PUT. On a 400 the server returns {error, key, detail}; we keep
  // the structured body so the detail can be rendered next to the offending key.
  // 403 (unknown/non-writable) and other errors surface via j.detail || j.error.
  async function putSettings(body) {
    const res = await fetch('/api/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body),
    });
    if (res.ok) return res.status === 204 ? null : res.json();
    let j = {};
    try { j = await res.json(); } catch (_) { /* non-JSON error */ }
    const err = new Error(j.detail || j.error || `${res.status}`);
    err.status = res.status;
    err.key = j.key || '';
    err.detail = j.detail || j.error || '';
    throw err;
  }

  function clearFieldErrors() {
    el.querySelectorAll('.cfg-err[data-err-key]').forEach((n) => {
      n.textContent = '';
      n.hidden = true;
    });
  }

  // Render a validation detail inline next to its key WITHOUT re-rendering, so the
  // user's in-progress form state is preserved (AC3).
  function showFieldError(key, detail) {
    const slot = el.querySelector(`.cfg-err[data-err-key="${key}"]`);
    if (slot) { slot.textContent = detail; slot.hidden = false; }
  }

  function wire() {
    // The Boards group is independent of the settings snapshot — wire it first so
    // it stays live even when the config load failed.
    wireBoards();
    // Failed load: no snapshot, so bind ONLY the Retry button and return before
    // any save handler exists — collect()/PUT is unreachable, so a click cannot
    // wipe the stored config.
    if (!loaded()) {
      const retry = el.querySelector('#cfgRetry');
      if (retry) retry.onclick = () => { msg = null; load(); };
      return;
    }
    // Per-secret 'clear': send an explicit JSON null for that one key.
    el.querySelectorAll('.cfg-clear[data-clear]').forEach((btn) => {
      btn.onclick = async () => {
        const key = btn.dataset.clear;
        clearFieldErrors();
        btn.disabled = true;
        try {
          await putSettings({ [key]: null });     // JSON null = explicit clear
          msg = { kind: 'ok', text: 'cleared' };
          await load();
        } catch (e) {
          btn.disabled = false;
          if (e.status === 400 && e.key) { showFieldError(e.key, e.detail); }
          else { msg = { kind: 'err', text: `clear failed: ${e.message}` }; render(); }
        }
      };
    });
    const save = el.querySelector('#cfgSave');
    if (!save) return;
    save.onclick = async () => {
      clearFieldErrors();
      const { body, error } = collect();
      if (error) { showFieldError(error.key, error.detail); return; }
      save.disabled = true;
      try {
        await putSettings(body);
        msg = { kind: 'ok', text: 'saved' };
        await load();                             // reload → reflect persisted values
      } catch (e) {
        save.disabled = false;
        if (e.status === 400 && e.key) {
          // Validation refusal: place the detail next to the key, keep form state.
          showFieldError(e.key, e.detail);
        } else {
          // 403 / other: surface the server's reason (j.detail || j.error).
          msg = { kind: 'err', text: `save failed: ${e.message}` };
          render();
        }
      }
    };
  }

  return { activate() { msg = null; load(); loadBoards(); } };
}
