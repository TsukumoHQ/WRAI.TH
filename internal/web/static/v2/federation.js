// Federation settings page — manage trusted peer relays (relay-to-relay DMs).
// Reads/writes /api/settings .federation. Tokens are shown masked; leaving a
// peer's token blank on save keeps the stored secret (backend merge). Address a
// remote agent as `name@label` to route a DM to that peer.
import { api } from './api.js';

export function initFederation(el, ctx) {
  const esc = ctx.esc;
  let source = 'settings';
  let peers = []; // {label,url,project,token_masked}
  let msg = null; // {kind:'ok'|'err', text}

  async function load() {
    try {
      const s = await api.settings();
      const f = (s && s.federation) || {};
      source = f.source || 'settings';
      peers = Array.isArray(f.peers) ? f.peers.slice() : [];
    } catch (e) {
      msg = { kind: 'err', text: `load failed: ${e.message}` };
    }
    render();
  }

  function rowHTML(p, i) {
    const ro = source === 'env' ? 'disabled' : '';
    return `
      <tr data-i="${i}">
        <td><input class="fd-in" data-k="label" value="${esc(p.label || '')}" placeholder="jerome" ${ro}></td>
        <td><input class="fd-in fd-url" data-k="url" value="${esc(p.url || '')}" placeholder="http://192.168.1.42:8090" ${ro}></td>
        <td><input class="fd-in fd-proj" data-k="project" value="${esc(p.project || 'default')}" ${ro}></td>
        <td><input class="fd-in fd-tok" data-k="token" value="" placeholder="${p.token_masked ? esc(p.token_masked) + ' (unchanged)' : 'shared secret'}" ${ro}></td>
        <td>${ro ? '' : `<button class="fd-del" data-i="${i}" title="remove" aria-label="remove peer">✕</button>`}</td>
      </tr>`;
  }

  function render() {
    const envLocked = source === 'env';
    const banner = envLocked
      ? `<div class="fd-note fd-note-warn">Configured via <code>RELAY_FEDERATION_PEERS</code> (env). Read-only here — unset the env var to manage peers from the dashboard.</div>`
      : `<div class="fd-note">Add the peer relay's address + a shared token. Configure the <em>same token</em> on both relays. Then message an agent there as <code>name@label</code>.</div>`;
    const banom = msg ? `<div class="fd-note ${msg.kind === 'err' ? 'fd-note-err' : 'fd-note-ok'}">${esc(msg.text)}</div>` : '';
    el.innerHTML = `
      <section class="fd-wrap">
        <header class="fd-head">
          <h2>Federation <span class="fd-state" data-on="${peers.length > 0}">${peers.length ? 'active' : 'off'}</span></h2>
          <p class="fd-sub">Relay-to-relay direct messaging. Peers below can DM your agents and receive DMs addressed <code>name@label</code>.</p>
        </header>
        ${banner}
        ${banom}
        <table class="fd-table">
          <thead><tr><th>Label</th><th>URL</th><th>Project</th><th>Token</th><th></th></tr></thead>
          <tbody id="fdRows">${peers.map(rowHTML).join('') || `<tr class="fd-empty"><td colspan="5">No peers configured.</td></tr>`}</tbody>
        </table>
        ${envLocked ? '' : `
        <div class="fd-actions">
          <button class="fd-add" id="fdAdd">+ Add peer</button>
          <button class="fd-save" id="fdSave">Save</button>
        </div>`}
      </section>`;
    wire();
  }

  function collect() {
    const out = [];
    el.querySelectorAll('#fdRows tr[data-i]').forEach((tr) => {
      const get = (k) => (tr.querySelector(`[data-k="${k}"]`)?.value || '').trim();
      const label = get('label');
      const url = get('url');
      if (!label && !url) return; // skip fully-empty row
      out.push({ label, url, project: get('project') || 'default', token: get('token') });
    });
    return out;
  }

  function wire() {
    const add = el.querySelector('#fdAdd');
    if (add) add.onclick = () => { peers = collect().concat({ label: '', url: '', project: 'default', token_masked: '' }); msg = null; render(); };
    el.querySelectorAll('.fd-del').forEach((b) => {
      b.onclick = () => { const cur = collect(); cur.splice(Number(b.dataset.i), 1); peers = cur.map((p) => ({ ...p, token_masked: '' })); msg = null; render(); };
    });
    const save = el.querySelector('#fdSave');
    if (save) save.onclick = async () => {
      const list = collect();
      // A brand-new peer (no stored token to inherit) needs a token now.
      for (const p of list) {
        if (!p.label || !p.url) { msg = { kind: 'err', text: 'each peer needs a label and a URL' }; return render(); }
      }
      save.disabled = true;
      try {
        await api.saveSettings({ federation_peers: JSON.stringify(list) });
        msg = { kind: 'ok', text: `saved — ${list.length} peer(s), hot-reloaded` };
        await load();
      } catch (e) {
        msg = { kind: 'err', text: `save failed: ${e.message}` };
        render();
      }
    };
  }

  return { activate() { msg = null; load(); } };
}
