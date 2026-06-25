// Messages — the orchestrator's comms desk. Agent-centric master/detail: a rail
// of agents (+ broadcast + all-traffic), a thread on the right, a composer to
// reply / DM / broadcast. Scope toggles between the current project and the
// whole fleet (cross-project), so you can reach any agent anywhere.
export function initMessages(root, ctx) {
  const { esc, colorFor, initialFor, fmtAgo } = ctx;
  const railList = root.querySelector('#msgRailList');
  const railSearch = root.querySelector('#msgRailSearch');
  const threadHead = root.querySelector('#msgThreadHead');
  const threadBody = root.querySelector('#msgThreadBody');
  const composer = root.querySelector('#msgComposer');
  const input = root.querySelector('#msgInput');
  const sendBtn = root.querySelector('#msgSend');
  const scopeWrap = root.querySelector('#msgScope');

  const ME = 'user';
  const ALL = '__all__';
  const BROADCAST = '__broadcast__';
  const SKIP = new Set(['', '*', 'system']);

  let msgs = [];
  let convs = [];                 // [{id,title,members[],message_count,...}]
  const convCache = new Map();    // conv id → messages[]
  let scopeMode = 'project';   // 'project' | 'all'
  let selected = ALL;
  let railFilter = '';
  let loadTok = 0;
  const isConv = (k) => typeof k === 'string' && k.startsWith('conv:');

  const projScope = () => ctx.selection; // current project, or 'all'
  const canProjectScope = () => ctx.selection && ctx.selection !== 'all';

  /* ---------------- data ---------------- */
  async function load() {
    const tok = ++loadTok;
    let list;
    try {
      list = scopeMode === 'all' ? await ctx.api.messagesAll() : await ctx.api.messages(projScope());
    } catch (_) { list = []; }
    let clist;
    try {
      clist = scopeMode === 'all' ? await ctx.api.conversationsAll() : await ctx.api.conversations(projScope());
    } catch (_) { clist = []; }
    if (tok !== loadTok) return;
    msgs = Array.isArray(list) ? list : [];
    convs = Array.isArray(clist) ? clist : [];
    convCache.clear();
    render();
  }

  // Lazily fetch + cache a conversation's full message thread, then re-render.
  async function selectKey(key) {
    selected = key;
    renderRail();
    if (isConv(key)) {
      const id = key.slice(5);
      if (!convCache.has(id)) {
        renderThread();                       // shows a loading/empty state meanwhile
        try {
          const m = await ctx.api.conversationMessages(id);
          convCache.set(id, Array.isArray(m) ? m : []);
        } catch (_) { convCache.set(id, []); }
      }
    }
    renderThread();
  }

  // Distinct agents seen in traffic, newest-active first.
  function agents() {
    const last = new Map();
    for (const m of msgs) {
      for (const who of [m.from, m.to]) {
        if (SKIP.has(who) || who === ME) continue;
        const t = Date.parse(m.created_at) || 0;
        if (!last.has(who) || t > last.get(who)) last.set(who, t);
      }
    }
    return [...last.entries()].sort((a, b) => b[1] - a[1]).map(([name]) => name);
  }

  function threadFor(key) {
    if (key === ALL) return msgs;
    if (key === BROADCAST) return msgs.filter((m) => m.to === '*');
    if (isConv(key)) return convCache.get(key.slice(5)) || [];
    return msgs.filter((m) => m.from === key || m.to === key);
  }

  function lastProjectForAgent(name) {
    for (const m of msgs) { // msgs arrive newest-first from the API
      if (m.from === name || m.to === name) return m.project;
    }
    return projScope();
  }

  /* ---------------- render ---------------- */
  function render() { renderRail(); renderThread(); }

  function renderRail() {
    // scope buttons
    scopeWrap.querySelectorAll('.scope-btn').forEach((b) => {
      b.classList.toggle('on', b.dataset.scope === scopeMode);
      if (b.dataset.scope === 'project') b.disabled = !canProjectScope();
    });

    const items = [];
    items.push(railRow(ALL, 'all traffic', `${msgs.length} msg${msgs.length === 1 ? '' : 's'}`, '#'));
    if (scopeMode === 'project' && canProjectScope()) {
      const bc = threadFor(BROADCAST).length;
      items.push(railRow(BROADCAST, 'broadcast', bc ? `${bc} sent` : 'message all', '*'));
    }
    // Conversations (multi-agent named threads) — their own group above the DMs.
    let cl = convs;
    if (railFilter) cl = cl.filter((c) => (c.title || '').toLowerCase().includes(railFilter));
    if (cl.length) {
      items.push('<div class="msg-rail-group">conversations</div>');
      for (const c of cl) items.push(convRow(c));
    }
    let list = agents();
    if (railFilter) list = list.filter((a) => a.toLowerCase().includes(railFilter));
    if (list.length) items.push('<div class="msg-rail-group">agents</div>');
    for (const a of list) {
      const th = threadFor(a);
      const lastMsg = th[0];
      const preview = lastMsg ? `${lastMsg.from === ME ? 'you: ' : ''}${(lastMsg.content || '').slice(0, 40)}` : '';
      items.push(railRow(a, a, preview, a, lastMsg, th.length));
    }
    railList.innerHTML = items.join('') || '<div class="empty">No agents in traffic</div>';
    railList.querySelectorAll('.msg-rail-row').forEach((el) =>
      el.addEventListener('click', () => selectKey(el.dataset.key)));
  }

  function convRow(c) {
    const key = 'conv:' + c.id;
    const title = c.title || 'untitled';
    const members = (c.members || []).length;
    const sub = `${members} member${members === 1 ? '' : 's'} · ${c.message_count || 0} msg`;
    return `<div class="msg-rail-row${selected === key ? ' on' : ''}" data-key="${esc(key)}" role="button" tabindex="0">
      <span class="msg-rail-glyph conv" aria-hidden="true">◇</span>
      <span class="msg-rail-main">
        <span class="msg-rail-name">${esc(title)}</span>
        <span class="msg-rail-sub">${esc(sub)}</span>
      </span>
      <span class="msg-rail-right"></span>
    </div>`;
  }

  function railRow(key, label, sub, avatarSeed, lastMsg, count) {
    const special = key === ALL || key === BROADCAST;
    const av = special
      ? `<span class="msg-rail-glyph" aria-hidden="true">${key === ALL ? '≡' : '◎'}</span>`
      : `<span class="msg-rail-av" style="background:${colorFor(avatarSeed)}">${esc(initialFor(avatarSeed))}</span>`;
    const time = lastMsg ? `<span class="msg-rail-time">${fmtAgo(lastMsg.created_at)}</span>` : '';
    const tag = (scopeMode === 'all' && lastMsg && lastMsg.project) ? `<span class="msg-proj-tag">${esc(lastMsg.project)}</span>` : '';
    return `<div class="msg-rail-row${selected === key ? ' on' : ''}" data-key="${esc(key)}" role="button" tabindex="0">
      ${av}
      <span class="msg-rail-main">
        <span class="msg-rail-name">${esc(label)}${count ? ` <b class="msg-rail-n">${count}</b>` : ''}</span>
        <span class="msg-rail-sub">${esc(sub)}</span>
      </span>
      <span class="msg-rail-right">${time}${tag}</span>
    </div>`;
  }

  function renderThread() {
    const conv = isConv(selected) ? convs.find((c) => 'conv:' + c.id === selected) : null;
    const isAgent = selected !== ALL && selected !== BROADCAST && !isConv(selected);
    let title; let sub;
    if (isConv(selected)) {
      title = (conv && conv.title) || 'conversation';
      sub = conv && conv.members && conv.members.length ? conv.members.join(', ') : 'conversation thread';
    } else if (selected === ALL) { title = 'all traffic'; sub = 'every message in scope (read-only)'; }
    else if (selected === BROADCAST) { title = 'broadcast to fleet'; sub = 'goes to every agent in this project'; }
    else { title = selected; sub = scopeMode === 'all' ? `in ${esc(lastProjectForAgent(selected))}` : 'direct message'; }
    threadHead.innerHTML = `<div class="msg-th-title">${esc(title)}</div><div class="msg-th-sub">${sub}</div>`;

    const list = threadFor(selected).slice().sort((a, b) => (Date.parse(a.created_at) || 0) - (Date.parse(b.created_at) || 0)).slice(-300);
    if (!list.length) {
      threadBody.innerHTML = `<div class="empty">${isConv(selected) ? 'Loading…' : 'No messages yet'}</div>`;
    } else {
      threadBody.innerHTML = list.map(bubble).join('');
      threadBody.scrollTop = threadBody.scrollHeight;
    }
    // composer: agent DM or broadcast; all-traffic + conversations are read-only here.
    const showComposer = selected === BROADCAST || isAgent;
    composer.hidden = !showComposer;
    if (showComposer) input.placeholder = selected === BROADCAST ? 'broadcast to all agents…' : `message ${selected}…`;
  }

  function bubble(m) {
    const mine = m.from === ME;
    const pr = (m.priority || '').toUpperCase();
    const prChip = (pr === 'P0' || pr === 'P1') ? `<span class="msg-pr ${pr.toLowerCase()}">${pr}</span>` : '';
    const tag = (scopeMode === 'all' && m.project) ? `<span class="msg-proj-tag">${esc(m.project)}</span>` : '';
    return `<div class="msg-bubble${mine ? ' mine' : ''}">
      <div class="msg-b-head">
        <span class="msg-b-av" style="background:${colorFor(m.from)}">${esc(initialFor(m.from))}</span>
        <span class="msg-b-from">${esc(m.from)}</span>
        <span class="msg-b-to">→ ${esc(m.to)}</span>
        ${prChip}${tag}
        <span class="msg-b-time">${fmtAgo(m.created_at)}</span>
      </div>
      <div class="msg-b-body">${esc(m.content || '')}</div>
    </div>`;
  }

  /* ---------------- send ---------------- */
  async function send() {
    const text = input.value.trim();
    if (!text) return;
    let to, sendProj;
    if (selected === BROADCAST) { to = '*'; sendProj = projScope(); }
    else { to = selected; sendProj = scopeMode === 'all' ? lastProjectForAgent(selected) : projScope(); }
    if (!sendProj || sendProj === 'all') { flash('pick a project to send'); return; }
    sendBtn.disabled = true;
    try {
      await ctx.api.sendMessage(sendProj, to, text);
      input.value = ''; autosize();
      await load();
    } catch (e) { flash(e.message || 'send failed'); }
    finally { sendBtn.disabled = false; }
  }
  function flash(msg) { threadHead.querySelector('.msg-th-sub').textContent = msg; }
  function autosize() { input.style.height = 'auto'; input.style.height = Math.min(input.scrollHeight, 140) + 'px'; }

  /* ---------------- wiring ---------------- */
  scopeWrap.addEventListener('click', (e) => {
    const b = e.target.closest('.scope-btn');
    if (!b || b.disabled) return;
    scopeMode = b.dataset.scope;
    selected = ALL;
    load();
  });
  railSearch.addEventListener('input', () => { railFilter = railSearch.value.trim().toLowerCase(); renderRail(); });
  composer.addEventListener('submit', (e) => { e.preventDefault(); send(); });
  input.addEventListener('input', autosize);
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); } });

  ctx.onEvent((evt) => {
    if (root.hidden) return;
    if (String(evt.type || '').startsWith('message')) load();
  });
  ctx.onSelection(() => {
    scopeMode = canProjectScope() ? 'project' : 'all';
    selected = ALL;
    if (!root.hidden) load();
  });

  return {
    activate() {
      scopeMode = canProjectScope() ? 'project' : 'all';
      load();
    },
  };
}
