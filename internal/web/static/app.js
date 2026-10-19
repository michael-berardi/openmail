/* OpenMail web app. No framework, no build step. All text is inserted with textContent. */
'use strict';

// The desktop app loads this page with ?desktop=1 so the layout can make room for the window controls.
try {
  if (new URLSearchParams(location.search).get('desktop') === '1') { localStorage.setItem('om_desktop', '1'); history.replaceState(null, '', '/'); }
  if (localStorage.getItem('om_desktop') === '1') document.documentElement.classList.add('desktop');
} catch (_) { /* storage blocked: browser layout */ }

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

const SVG = {
  inbox: 'M3 13l3-8h12l3 8v6H3zM3 13h5l1 3h6l1-3h5',
  unread: 'M12 7a5 5 0 100 10 5 5 0 000-10z',
  flag: 'M5 21V4M5 4h12l-2 4 2 4H5',
  star: 'M12 3l2.7 5.6 6.1.9-4.4 4.3 1 6.1L12 17l-5.4 2.9 1-6.1L3.2 9.5l6.1-.9z',
  clock: 'M12 3a9 9 0 100 18 9 9 0 000-18zM12 7v5l3 2',
  send: 'M3 11l18-8-8 18-2-8z',
  draft: 'M5 3h10l4 4v14H5zM15 3v4h4M8 12h8M8 16h6',
  archive: 'M3 5h18v4H3zM5 9v10h14V9M10 13h4',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13',
  junk: 'M12 3l9 16H3zM12 10v4M12 17h.01',
  folder: 'M3 6h6l2 2h10v11H3z',
  reply: 'M10 8l-6 5 6 5M4 13h9a7 7 0 017 7',
  replyall: 'M8 8l-5 5 5 5M13 8l-5 5 5 5M8 13h6a7 7 0 017 7',
  forward: 'M14 8l6 5-6 5M20 13h-9a7 7 0 00-7 7',
  mail: 'M3 6h18v12H3zM3 6l9 7 9-7',
  read: 'M3 9l9-6 9 6v10H3zM3 9l9 6 9-6',
  snooze: 'M12 3a9 9 0 100 18 9 9 0 000-18zM12 8v4h4',
  back: 'M15 5l-7 7 7 7',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  attach: 'M20 11l-8 8a5 5 0 01-7-7l8-8a3.5 3.5 0 015 5l-8 8a2 2 0 01-3-3l7-7',
};
function icon(name, cls) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('aria-hidden', 'true');
  if (cls) s.setAttribute('class', cls);
  const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  p.setAttribute('d', SVG[name] || SVG.folder);
  s.append(p);
  return s;
}

// ---------- api ----------
async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', credentials: 'same-origin', headers: { 'X-OpenMail': '1' } };
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  let res;
  try { res = await fetch(path, init); }
  catch (e) { throw new Error('Cannot reach OpenMail (is `openmail serve` still running?)'); }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = text; }
  if (!res.ok) {
    if (res.status === 401) {
      document.body.replaceChildren(h('main', { style: { padding: '3rem', maxWidth: '36rem', margin: 'auto' } },
        h('h1', { text: 'Sign in required' }),
        h('p', { text: 'Open the address printed by `openmail serve` (it includes your sign-in token).' })));
    }
    throw new Error((data && data.error) || `Request failed (${res.status})`);
  }
  return data;
}

// ---------- state ----------
const S = {
  ov: null, settings: {}, drafts: [],
  view: { kind: 'query', q: 'in:inbox', title: 'All Inboxes', key: 'smart:inbox' },
  threads: [], offset: 0, more: false, sel: null, thread: [], loadingList: false,
  page: null, keys: {}, pendingKey: '', pendingTimer: null, markTimer: null, listSeq: 0,
};
const PAGE = 40;

// ---------- utilities ----------
function announce(msg) { const l = $('#live'); l.textContent = ''; setTimeout(() => { l.textContent = msg; }, 30); }

function toast(msg, opts = {}) {
  const t = h('div', { class: 'toast' + (opts.error ? ' err' : ''), role: opts.error ? 'alert' : 'status' }, h('span', { text: msg }));
  let timer;
  const close = () => { clearTimeout(timer); t.remove(); };
  if (opts.action) t.append(h('button', { text: opts.action.label, onclick: () => { close(); opts.action.run(); } }));
  t.append(h('button', { 'aria-label': 'Dismiss', text: '✕', onclick: close }));
  const dlg = document.querySelector('dialog[open]');
  let host = $('#toasts');
  if (dlg) { host = $('.toasts', dlg) || dlg.appendChild(h('div', { class: 'toasts', role: 'region', 'aria-label': 'Notifications' })); }
  host.append(t);
  timer = setTimeout(close, opts.ms || (opts.error ? 9000 : 5000));
  return { close, el: t };
}
const fail = (e) => { console.error(e); toast(e.message || String(e), { error: true }); };

function fmtDate(ts, long) {
  const d = new Date(ts * 1000), now = new Date();
  if (long) return d.toLocaleString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  if (now - d < 20 * 3600e3 && now.getDate() === d.getDate()) return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  return d.toLocaleDateString();
}
const addrText = (a) => (a.name ? `${a.name} <${a.addr}>` : a.addr);
const addrList = (l) => (l || []).map(addrText).join(', ');
const who = (a) => a.name || a.addr;
function hue(s) { let x = 0; for (const c of s) x = (x * 31 + c.charCodeAt(0)) >>> 0; return x % 360; }
function accountColor(id) { const a = (S.ov?.accounts || []).find((x) => x.id === id); return a ? a.color : 'var(--accent)'; }
function selfAddrs() { return new Set((S.ov?.accounts || []).map((a) => a.email.toLowerCase())); }
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };

// ---------- settings -> appearance ----------
function applySettings() {
  const s = S.settings, root = document.documentElement;
  if (!s['ui.theme'] || s['ui.theme'] === 'system') root.removeAttribute('data-theme'); else root.setAttribute('data-theme', s['ui.theme']);
  root.setAttribute('data-density', s['ui.density'] || 'comfortable');
  root.style.setProperty('--scale', s['ui.font_scale'] || '1');
  root.style.setProperty('--lines', s['ui.preview_lines'] || '2');
  const acc = s['ui.accent'] || '#0a84ff';
  root.style.setProperty('--accent', acc);
  root.style.setProperty('--accent-fg', luminance(acc) > 0.45 ? '#000' : '#fff');
  if (s['ui.reduce_motion'] === 'reduce') root.setAttribute('data-motion', 'reduce'); else root.removeAttribute('data-motion');
  $('#app').setAttribute('data-layout', s['ui.layout'] || 'three-pane');
  let custom = {};
  try { custom = s['ui.keybindings'] ? JSON.parse(s['ui.keybindings']) : {}; } catch (_) { toast('ui.keybindings is not valid JSON; using defaults', { error: true }); }
  S.keys = Object.assign({}, DEFAULT_KEYS, custom);
}
function luminance(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex); if (!m) return 0;
  const n = parseInt(m[1], 16), c = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((v) => { v /= 255; return v <= .03928 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4; });
  return .2126 * c[0] + .7152 * c[1] + .0722 * c[2];
}
async function putSetting(key, value) {
  S.settings = await api('/api/settings', { method: 'PUT', body: { key, value: String(value) } });
  applySettings();
  if (/^ui\.(theme|dark_mail|font_scale)$/.test(key) && S.thread.length) renderReader();
}

// ---------- sidebar ----------
const SMART = [
  { key: 'smart:inbox', title: 'All Inboxes', q: 'in:inbox', icon: 'inbox', count: () => inboxUnread() },
  { key: 'smart:unread', title: 'Unread', q: 'is:unread in:inbox', icon: 'unread' },
  { key: 'smart:flagged', title: 'Flagged', q: 'is:flagged', icon: 'flag' },
  { key: 'smart:vip', title: 'VIP', q: 'is:vip', icon: 'star' },
  { key: 'smart:snoozed', title: 'Snoozed', q: 'is:snoozed', icon: 'clock' },
  { key: 'smart:sent', title: 'Sent', q: 'in:sent', icon: 'send' },
];
const ROLE_ICON = { inbox: 'inbox', sent: 'send', drafts: 'draft', trash: 'trash', junk: 'junk', archive: 'archive', all: 'archive' };
const ROLE_LABEL = { inbox: 'Inbox', sent: 'Sent', drafts: 'Drafts', trash: 'Trash', junk: 'Junk', archive: 'Archive' };
function inboxUnread() { let n = 0; for (const a of S.ov?.accounts || []) for (const m of a.mailboxes || []) if (m.role === 'inbox') n += m.unread; return n; }
const quoteName = (n) => (/[\s:"]/.test(n) ? `"${n.replace(/"/g, '')}"` : n);

function renderSidebar() {
  const side = $('#side');
  side.replaceChildren();
  const item = (opts) => {
    const b = h('button', { class: 'side-item', 'aria-current': S.view.key === opts.key ? 'page' : false, onclick: () => { closeMenu(); opts.go ? opts.go() : navigate({ kind: 'query', q: opts.q, title: opts.title, key: opts.key, acct: opts.acct, mbName: opts.mbName }); } },
      icon(opts.icon, 'ico'), h('span', { class: 'name', text: opts.title }));
    const c = opts.count ? opts.count() : 0;
    if (c > 0) b.append(h('span', { class: 'badge' + (opts.hot ? ' hot' : ''), text: String(c), 'aria-label': `${c} unread` }));
    return b;
  };
  for (const s of SMART) side.append(item(s));
  const dn = (S.ov?.needs_approval || 0) + draftCount();
  side.append(item({ key: 'page:drafts', title: 'Drafts', icon: 'draft', count: () => dn, hot: (S.ov?.needs_approval || 0) > 0, go: () => showPage('drafts') }));
  for (const a of S.ov?.accounts || []) {
    const g = h('div', { class: 'side-group' }, h('span', { class: 'dotc', style: { background: a.color || 'var(--accent)' } }), h('span', { class: 'gname', title: a.email, text: a.name && a.name !== a.email ? a.name : a.email }));
    if (!a.enabled) g.append(h('span', { class: 'pill', text: 'off' }));
    side.append(g);
    for (const m of a.mailboxes || []) {
      if (!m.subscribed) continue;
      const key = `mb:${m.id}`;
      const title = ROLE_LABEL[m.role] || m.name;
      side.append(item({ key, title, acct: a.id, mbName: m.name, icon: ROLE_ICON[m.role] || 'folder', q: `account:${a.id} in:${quoteName(m.name)}`, count: () => (m.role === 'trash' || m.role === 'junk' || m.role === 'sent' || m.role === 'drafts' ? 0 : m.unread) }));
    }
  }
  if (!(S.ov?.accounts || []).length) side.append(h('div', { class: 'empty' }, h('p', { text: 'No accounts yet.' }), h('button', { class: 'btn primary', text: 'Add account', onclick: () => addAccountDialog() })));
  const nb = $('#b-approve');
  const na = S.ov?.needs_approval || 0;
  nb.hidden = na === 0; nb.textContent = String(na); nb.setAttribute('aria-label', `${na} waiting for approval`);
  renderStatus();
  document.title = (inboxUnread() > 0 ? `(${inboxUnread()}) ` : '') + 'OpenMail';
}
const draftCount = () => S.drafts.filter((d) => d.state === 'draft').length;

function renderStatus() {
  const box = $('#syncstatus');
  box.replaceChildren();
  const st = S.ov?.status || [];
  const err = st.find((x) => x.state === 'error' && x.error);
  const busy = st.find((x) => x.state === 'syncing' || x.state === 'connecting');
  if (err) box.append(h('span', { class: 'err', text: `${err.account}: ${err.error}` }));
  else if (busy) box.append(`Syncing ${busy.account}${busy.progress ? ' · ' + busy.progress : ''}`);
  else if (st.length) { const t = Math.max(...st.map((x) => x.last_sync || 0)); if (t) box.append(`Updated ${fmtDate(t)}`); }
  if (S.ov?.pending_ops) box.append(h('div', { text: `${S.ov.pending_ops} change(s) waiting to reach the server` }));
}

function closeMenu() { $('#app').removeAttribute('data-menu'); }

// ---------- navigation ----------
function navigate(view) {
  S.view = view; S.sel = null; S.thread = []; S.offset = 0; S.page = null;
  $('#app').removeAttribute('data-page');
  $('#app').setAttribute('data-pane', 'list');
  $('#q').value = view.kind === 'search' ? view.q : '';
  renderSidebar(); renderReader(); loadList();
}

async function loadList(append = false) {
  const seq = ++S.listSeq;
  try {
    const q = S.view.q;
    const useThreads = S.settings['ui.threads'] !== 'false';
    const off = append ? S.offset : 0;
    let rows;
    if (useThreads) rows = await api(`/api/threads?q=${encodeURIComponent(q)}&limit=${PAGE}&offset=${off}`);
    else rows = (await api(`/api/messages?q=${encodeURIComponent(q)}&limit=${PAGE}&offset=${off}`)).map((m) => ({ thread_id: m.thread_id, latest: m, count: 1, unread: m.seen ? 0 : 1, flagged: m.flagged, senders: [who(m.from)] }));
    if (seq !== S.listSeq) return; // a newer request superseded this one
    S.threads = append ? S.threads.concat(rows) : rows;
    S.offset = off + rows.length; S.more = rows.length === PAGE;
    renderList();
    syncOpenThread();
  } catch (e) { fail(e); }
}

// If the open conversation grew (new reply arrived), reload it.
function syncOpenThread() {
  if (!S.sel) return;
  const row = S.threads.find((t) => t.thread_id === S.sel);
  if (row && S.thread.length && row.latest.id !== S.thread[S.thread.length - 1].id && !LIST_ROLES.has(row.latest.mailbox_role)) selectThread(S.sel, false);
}

function renderList() {
  const list = $('#list');
  const prevScroll = list.scrollTop;
  list.replaceChildren();
  const title = $('#viewtitle');
  title.replaceChildren(h('span', { text: S.view.title || 'Search' }));
  const unread = S.threads.reduce((n, t) => n + (t.unread ? 1 : 0), 0);
  if (S.threads.length) title.append(h('small', { text: unread ? `${unread} unread` : `${S.threads.length}${S.more ? '+' : ''} conversation${S.threads.length === 1 && !S.more ? '' : 's'}` }));
  if (!S.threads.length) {
    list.append(h('div', { class: 'empty', role: 'presentation' }, S.ov && !S.ov.accounts.length ? 'Add an account to get started.' : 'Nothing here.'));
    return;
  }
  for (const t of S.threads) list.append(rowFor(t));
  if (S.more) list.append(h('div', { class: 'more', role: 'presentation' }, h('button', { class: 'btn', text: 'Show more', onclick: () => loadList(true) })));
  else if (S.view.mbName) list.append(h('div', { class: 'more', role: 'presentation' }, h('button', { class: 'btn', text: 'Download older mail from the server', onclick: async (e) => { e.target.disabled = true; try { const r = await api('/api/load-older', { method: 'POST', body: { account: S.view.acct, mailbox: S.view.mbName, count: 200 } }); toast(r.loaded ? `Downloaded ${r.loaded} older message(s)` : 'No older mail on the server'); if (r.loaded) { refreshOverview(); loadList(); } } catch (er) { fail(er); } e.target.disabled = false; } })));
  list.scrollTop = prevScroll;
  if (S.sel) markSelected(); else if (!S.thread.length) renderReader();
}

function qbtn(name, label, fn) {
  return h('button', { class: 'qb', type: 'button', tabindex: '-1', title: label, 'aria-hidden': 'true', onclick: (e) => { e.stopPropagation(); fn(); } }, icon(name));
}

async function quick(t, kind) {
  try {
    const msgs = await api('/api/thread/' + encodeURIComponent(t.thread_id));
    const all = msgs.map((m) => m.id);
    const live = msgs.filter((m) => !LIST_ROLES.has(m.mailbox_role)).map((m) => m.id);
    const useIds = ['flag', 'unflag', 'read', 'unread'].includes(kind) ? all : (live.length ? live : all);
    await api('/api/act', { method: 'POST', body: { ids: useIds, action: kind } });
    if (kind === 'archive' || kind === 'trash') {
      announce(kind === 'archive' ? 'Archived' : 'Moved to Trash');
      toast(kind === 'archive' ? 'Archived' : 'Moved to Trash', { action: { label: 'Undo', run: () => undo(msgs.filter((m) => useIds.includes(m.id)), kind) } });
      if (S.sel === t.thread_id) { S.sel = null; S.thread = []; renderReader(); }
      S.threads = S.threads.filter((x) => x.thread_id !== t.thread_id); renderList();
    } else { loadList(); if (S.sel === t.thread_id) selectThread(t.thread_id, false); }
    refreshOverview();
  } catch (e) { fail(e); }
}

function rowFor(t) {
  const m = t.latest;
  const label = [t.unread ? 'Unread' : 'Read', t.senders.join(', '), m.subject || '(no subject)', fmtDate(m.date), t.count > 1 ? `${t.count} messages` : '', m.has_attachments ? 'has attachment' : '', t.flagged ? 'flagged' : '', m.vip ? 'VIP' : ''].filter(Boolean).join(', ');
  const multi = (S.ov?.accounts || []).length > 1;
  const row = h('div', { class: 'row' + (t.unread ? ' unread' : ''), role: 'option', id: 'r-' + t.thread_id, 'data-tid': t.thread_id, 'aria-selected': 'false', 'aria-label': label },
    h('span', { class: 'dot', 'aria-hidden': 'true' }),
    h('div', { class: 'r1' },
      h('span', { class: 'from' }, multi ? h('span', { class: 'acct', style: { background: accountColor(m.account_id) }, 'aria-hidden': 'true' }) : null, m.vip ? icon('star', 'ic vip') : null, t.senders.join(', '), t.count > 1 ? h('span', { class: 'cnt', text: String(t.count) }) : null),
      h('time', { text: fmtDate(m.date), datetime: new Date(m.date * 1000).toISOString() })),
    h('div', { class: 'r2' }, h('span', { class: 'subj', text: m.subject || '(no subject)' }),
      h('span', { class: 'icons', 'aria-hidden': 'true' }, t.flagged ? icon('flag', 'ic flag') : null, m.has_attachments ? icon('attach', 'ic') : null)),
    h('div', { class: 'r3 preview', text: m.snippet || '' }),
    h('div', { class: 'quick' }, qbtn('archive', 'Archive', () => quick(t, 'archive')), qbtn('trash', 'Trash', () => quick(t, 'trash')), qbtn('flag', t.flagged ? 'Unflag' : 'Flag', () => quick(t, t.flagged ? 'unflag' : 'flag')), qbtn('mail', t.unread ? 'Mark read' : 'Mark unread', () => quick(t, t.unread ? 'read' : 'unread'))));
  row.addEventListener('click', () => selectThread(t.thread_id, true));
  return row;
}

function markSelected() {
  $$('#list .row').forEach((r) => r.setAttribute('aria-selected', r.dataset.tid === S.sel ? 'true' : 'false'));
  const cur = S.sel ? $('#r-' + CSS.escape(S.sel)) : null;
  $('#list').setAttribute('aria-activedescendant', cur ? cur.id : '');
  if (cur) cur.scrollIntoView({ block: 'nearest' });
}

async function selectThread(tid, focusReader) {
  S.sel = tid; markSelected();
  $('#app').setAttribute('data-pane', 'reader');
  try {
    const msgs = await api('/api/thread/' + encodeURIComponent(tid));
    if (S.sel !== tid) return; // the user moved on
    S.thread = msgs;
    renderReader();
    scheduleMarkRead();
    if (focusReader) $('#thread').scrollTop = 0;
  } catch (e) { fail(e); }
}

function scheduleMarkRead() {
  clearTimeout(S.markTimer);
  const secs = parseFloat(S.settings['ui.mark_read_seconds'] || '1');
  const unread = S.thread.filter((m) => !m.seen).map((m) => m.id);
  if (!unread.length) return;
  const tid = S.sel;
  S.markTimer = setTimeout(async () => {
    if (S.sel !== tid) return;
    try { await api('/api/act', { method: 'POST', body: { ids: unread, action: 'read' } }); S.thread.forEach((m) => { m.seen = true; }); const row = S.threads.find((t) => t.thread_id === tid); if (row) row.unread = 0; renderList(); refreshOverview(); } catch (e) { fail(e); }
  }, Math.max(0, secs) * 1000);
}

// ---------- reader ----------
const LIST_ROLES = new Set(['sent', 'drafts', 'trash', 'junk']);
function actionable() { const a = S.thread.filter((m) => !LIST_ROLES.has(m.mailbox_role)); return a.length ? a : S.thread; }

function renderReader() {
  const acts = $('#actions'), box = $('#thread');
  acts.replaceChildren(); box.replaceChildren();
  if (!S.thread.length) {
    acts.hidden = true;
    box.append(h('div', { class: 'empty big' }, icon('mail', 'bigico'), h('div', { text: S.threads.length ? 'No conversation selected' : '' })));
    return;
  }
  acts.hidden = false;
  const btn = (name, label, fn, cls) => h('button', { class: 'btn icon ' + (cls || ''), title: label, 'aria-label': label, onclick: fn }, icon(name));
  acts.append(
    h('button', { class: 'btn icon', id: 'back', 'aria-label': 'Back to list', onclick: () => $('#app').setAttribute('data-pane', 'list') }, icon('back')),
    btn('reply', 'Reply (r)', () => respond('reply')),
    btn('replyall', 'Reply all (a)', () => respond('all')),
    btn('forward', 'Forward (f)', () => respond('forward')),
    h('span', { class: 'spacer' }),
    btn('archive', 'Archive (e)', () => act('archive')),
    btn('trash', 'Move to Trash (#)', () => act('trash')),
    btn('junk', 'Junk (!)', () => act('junk')),
    btn('flag', 'Flag (s)', () => toggleFlag()),
    btn('mail', 'Mark read or unread (u)', () => toggleRead()),
    btn('snooze', 'Snooze (z)', (e) => snoozeMenu(e.currentTarget)),
    btn('folder', 'Move to…', (e) => moveMenu(e.currentTarget)),
  );
  const last = S.thread[S.thread.length - 1];
  box.append(h('h2', { text: last.subject || '(no subject)' }));
  S.thread.forEach((m, i) => box.append(messageCard(m, i === S.thread.length - 1 || !m.seen)));
}

function messageCard(m, open) {
  const card = h('article', { class: 'msg' + (open ? '' : ' collapsed'), 'aria-label': `Message from ${who(m.from)}` });
  const head = h('button', { class: 'head', 'aria-expanded': open ? 'true' : 'false', onclick: () => { const c = card.classList.toggle('collapsed'); head.setAttribute('aria-expanded', String(!c)); if (!c && !card.dataset.loaded) fillBody(card, m); } },
    h('span', { class: 'avatar', 'aria-hidden': 'true', style: { background: `hsl(${hue(m.from.addr)} 55% 38%)` }, text: (who(m.from)[0] || '?').toUpperCase() }),
    h('span', { class: 'who' },
      h('div', { class: 'name', text: who(m.from) + (m.vip ? ' ★' : '') }),
      h('div', { class: 'meta', text: `${m.from.name ? m.from.addr + ' · ' : ''}to ${addrList(m.to) || '(undisclosed)'}${m.cc?.length ? ' · cc ' + addrList(m.cc) : ''}` }),
      h('div', { class: 'snip', text: m.snippet })),
    h('time', { class: 'when', text: fmtDate(m.date, true) }));
  card.append(head);
  if (open) fillBody(card, m);
  return card;
}

function securityNotes(m) {
  const notes = [], ar = (m.authentication_results || '').toLowerCase();
  if (/(spf|dkim|dmarc)=fail/.test(ar)) notes.push('Sender authentication failed (SPF, DKIM or DMARC). This message may not be from who it says.');
  if (m.reply_to?.length && m.reply_to[0].addr.toLowerCase() !== m.from.addr.toLowerCase()) notes.push(`Replies go to ${m.reply_to[0].addr}, not ${m.from.addr}.`);
  return notes;
}

function fillBody(card, m) {
  card.dataset.loaded = '1';
  for (const n of securityNotes(m)) card.append(h('div', { class: 'security', role: 'note', text: '⚠ ' + n }));
  if (m.list_unsubscribe) {
    card.append(h('div', { class: 'banner info' }, 'This message is from a mailing list.',
      h('button', { class: 'btn', text: 'Unsubscribe', onclick: () => unsubscribe(m) })));
  }
  const body = h('div', { class: 'body' });
  card.append(body);
  if (m.mailbox_role === 'drafts') {
    card.insertBefore(h('div', { class: 'banner info' }, 'This is a draft saved on the server.', h('button', { class: 'btn', text: 'Edit draft', onclick: async () => { try { const d = await api(`/api/message/${m.id}/edit-draft`, { method: 'POST' }); openCompose(await api('/api/drafts?id=' + d.id)); loadDrafts(); } catch (e) { fail(e); } } })), body);
  }
  if (m.partial) {
    const b = h('div', { class: 'banner info' }, `Large message (${fmtSize(m.size)}), not downloaded.`, h('button', { class: 'btn', text: 'Download', onclick: async (e) => {
      e.target.disabled = true; e.target.textContent = 'Downloading…';
      try { await api(`/api/message/${m.id}/download`, { method: 'POST' }); if (S.sel) S.thread = await api('/api/thread/' + encodeURIComponent(S.sel)); renderReader(); } catch (er) { fail(er); e.target.disabled = false; e.target.textContent = 'Download'; }
    } }));
    card.insertBefore(b, body);
  }
  if (m.partial) { /* header-only stub: nothing to render yet */ } else if (m.has_html) {
    const f = h('iframe', { title: 'Message from ' + who(m.from), sandbox: 'allow-same-origin allow-popups allow-popups-to-escape-sandbox', referrerpolicy: 'no-referrer', loading: 'eager' });
    const fit = () => { try { const d = f.contentDocument; f.style.height = Math.max(40, Math.ceil(d.body.getBoundingClientRect().height) + 16) + 'px'; } catch (_) { /* cross-origin never happens here */ } };
    f.addEventListener('load', () => {
      try { f.contentDocument.body.style.zoom = S.settings['ui.font_scale'] || '1'; } catch (_) { /* unreadable frame */ }
      darkMail(f, card);
      fit(); setTimeout(fit, 300); setTimeout(fit, 1200);
      try {
        if (f.contentDocument.querySelector('img[data-blocked-src]')) showRemoteBanner(card, m, f);
        const bad = linkMismatch(f.contentDocument);
        if (bad && !card.querySelector('.linkwarn')) card.insertBefore(h('div', { class: 'security linkwarn', role: 'note', text: `⚠ A link shows ${bad.shown} but goes to ${bad.host}. Check before clicking.` }), card.querySelector('.body'));
        f.contentDocument.addEventListener('click', (e) => { const a = e.target.closest && e.target.closest('a[href]'); if (a && /^mailto:/i.test(a.href)) { e.preventDefault(); composeTo(a.href.slice(7).split('?')[0]); } });
      } catch (_) { /* ignore */ }
    });
    f.src = `/api/message/${m.id}/html`;
    body.append(f);
  } else {
    body.append(plainBody(m.text || ''));
  }
  const atts = (m.attachments || []).filter((a) => !(a.inline && !a.filename));
  if (atts.length) {
    const box = h('div', { class: 'atts' });
    for (const a of atts) {
      const isImg = /^image\/(png|jpe?g|gif|webp)$/.test(a.content_type);
      box.append(h('a', { class: 'att', href: `/api/message/${m.id}/attachment/${a.index}`, download: a.filename || 'attachment', 'aria-label': `Download ${a.filename || 'attachment'}, ${fmtSize(a.size)}` },
        isImg ? h('img', { src: `/api/message/${m.id}/attachment/${a.index}?inline=1`, alt: '' }) : icon('attach'),
        h('span', { text: `${a.filename || 'attachment'} · ${fmtSize(a.size)}` })));
    }
    card.append(box);
  }
}

function isDarkUI() {
  const t = document.documentElement.getAttribute('data-theme');
  return t === 'dark' || t === 'high-contrast' || (!t && matchMedia('(prefers-color-scheme: dark)').matches);
}
// Message bodies are authored for white paper. In a dark UI we invert them, then put images back.
function darkMail(frame, card) {
  if (S.settings['ui.dark_mail'] === 'false' || !isDarkUI()) return;
  try {
    const d = frame.contentDocument;
    const st = d.createElement('style');
    st.textContent = 'html,body{background:#e9e9eb!important}img,video,picture,svg image{filter:invert(1) hue-rotate(180deg)}';
    d.head.append(st);
    frame.style.filter = 'invert(1) hue-rotate(180deg)';
    frame.dataset.dark = '1';
    if (!card.querySelector('.dm')) {
      const b = h('button', { class: 'btn tiny dm', type: 'button', 'aria-pressed': 'true', title: 'Show this message in its original colours', text: 'Original colours', onclick: () => {
        const on = b.getAttribute('aria-pressed') === 'true';
        frame.style.filter = on ? 'none' : 'invert(1) hue-rotate(180deg)';
        st.disabled = on;
        b.setAttribute('aria-pressed', String(!on));
      } });
      card.querySelector('.body').prepend(b);
    }
  } catch (_) { /* frame not readable: leave it light */ }
}

// A link whose visible text is an address but whose target is another site is a classic phishing trick.
function linkMismatch(doc) {
  for (const a of doc.querySelectorAll('a[href^="http"]')) {
    const txt = (a.textContent || '').trim();
    const m = /^(?:https?:\/\/)?((?:[a-z0-9-]+\.)+[a-z]{2,})(?:[\/:?#]|$)/i.exec(txt);
    if (!m) continue;
    let host; try { host = new URL(a.href).hostname.toLowerCase(); } catch (_) { continue; }
    const shown = m[1].toLowerCase().replace(/^www\./, ''), real = host.replace(/^www\./, '');
    if (shown !== real && !real.endsWith('.' + shown) && !shown.endsWith('.' + real)) return { shown, host };
  }
  return null;
}

function showRemoteBanner(card, m, frame) {
  if (card.querySelector('.banner.remote')) return;
  const b = h('div', { class: 'banner remote' }, 'Remote images blocked.',
    h('button', { class: 'btn', text: 'Load images', onclick: () => { frame.src = `/api/message/${m.id}/html?remote=1`; b.remove(); } }),
    h('button', { class: 'btn', text: `Always load from ${m.from.addr}`, onclick: async () => { try { await api('/api/allow-remote', { method: 'POST', body: { addr: m.from.addr } }); frame.src = `/api/message/${m.id}/html`; b.remove(); } catch (e) { fail(e); } } }));
  card.insertBefore(b, card.querySelector('.body'));
}

const fmtSize = (n) => (n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(0) + ' KB' : (n / 1048576).toFixed(1) + ' MB');

function plainBody(text) {
  const root = h('div', { class: 'plain' });
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  let quote = null;
  const urlRe = /(https?:\/\/[^\s<>"')]+[^\s<>"').,;:!?])/g;
  const addText = (parent, s) => {
    let last = 0, m;
    urlRe.lastIndex = 0;
    while ((m = urlRe.exec(s))) {
      parent.append(s.slice(last, m.index));
      parent.append(h('a', { href: m[1], target: '_blank', rel: 'noopener noreferrer', text: m[1] }));
      last = m.index + m[0].length;
    }
    parent.append(s.slice(last));
  };
  for (const line of lines) {
    if (/^\s*>/.test(line)) {
      if (!quote) { quote = h('span', { class: 'quote' }); root.append(quote); }
      addText(quote, line.replace(/^\s*>+ ?/, '') + '\n');
    } else { quote = null; addText(root, line + '\n'); }
  }
  return root;
}

// ---------- message actions ----------
function ids(msgs) { return msgs.map((m) => m.id); }

async function act(kind, value, opts = {}) {
  if (!S.thread.length) return;
  const msgs = opts.all ? S.thread : actionable();
  const nextTid = neighbour();
  try {
    await api('/api/act', { method: 'POST', body: { ids: ids(msgs), action: kind, value: value || '' } });
    const verb = { archive: 'Archived', trash: 'Moved to Trash', junk: 'Moved to Junk', move: 'Moved', snooze: 'Snoozed', read: 'Marked read', unread: 'Marked unread', flag: 'Flagged', unflag: 'Unflagged' }[kind] || 'Done';
    const gone = ['archive', 'trash', 'junk', 'move', 'snooze'].includes(kind);
    announce(verb);
    if (gone) {
      toast(`${verb}${kind === 'move' && value ? ' to ' + value : ''}`, kind === 'archive' || kind === 'trash' ? { action: { label: 'Undo', run: () => undo(msgs, kind) } } : {});
      S.threads = S.threads.filter((t) => t.thread_id !== S.sel);
      S.thread = []; S.sel = null;
      renderList(); renderReader();
      if (nextTid) selectThread(nextTid, false); else $('#app').setAttribute('data-pane', 'list');
    } else {
      S.thread = await api('/api/thread/' + encodeURIComponent(S.sel));
      renderReader(); loadList();
    }
    refreshOverview();
  } catch (e) { fail(e); }
}

async function undo(msgs, kind) {
  try {
    // put every message back in the folder it came from
    const by = new Map();
    for (const m of msgs) { const k = m.mailbox || 'inbox'; if (!by.has(k)) by.set(k, []); by.get(k).push(m.id); }
    for (const [folder, list] of by) await api('/api/act', { method: 'POST', body: { ids: list, action: 'move', value: folder } });
    announce('Undone');
    refreshOverview(); loadList();
  } catch (e) { fail(e); }
}

function neighbour() {
  const i = S.threads.findIndex((t) => t.thread_id === S.sel);
  if (i < 0) return null;
  return (S.threads[i + 1] || S.threads[i - 1] || {}).thread_id || null;
}

function toggleFlag() { const flagged = S.thread.some((m) => m.flagged); act(flagged ? 'unflag' : 'flag', '', { all: true }); }
function toggleRead() { const anyUnread = S.thread.some((m) => !m.seen); act(anyUnread ? 'read' : 'unread', '', { all: true }); }

function popMenu(anchor, items) {
  $$('.menu').forEach((m) => m.remove());
  const r = anchor.getBoundingClientRect();
  const m = h('div', { class: 'menu', role: 'menu', style: { top: Math.min(r.bottom + 4, innerHeight - 40 * items.length - 20) + 'px', left: Math.min(r.left, innerWidth - 220) + 'px' } },
    items.map((it) => h('button', { role: 'menuitem', text: it.label, onclick: () => { m.remove(); it.run(); } })));
  document.body.append(m);
  const first = $('button', m); if (first) first.focus();
  const close = (e) => { if (e.type === 'keydown' && e.key !== 'Escape') return; if (e.type === 'click' && m.contains(e.target)) return; m.remove(); document.removeEventListener('click', close, true); document.removeEventListener('keydown', close, true); anchor.focus && anchor.focus(); };
  setTimeout(() => { document.addEventListener('click', close, true); document.addEventListener('keydown', close, true); }, 0);
  m.addEventListener('keydown', (e) => { const b = $$('button', m), i = b.indexOf(document.activeElement); if (e.key === 'ArrowDown') { e.preventDefault(); b[(i + 1) % b.length].focus(); } if (e.key === 'ArrowUp') { e.preventDefault(); b[(i - 1 + b.length) % b.length].focus(); } });
}

function snoozeMenu(anchor) {
  popMenu(anchor, [['Later today', '3h'], ['Tonight', 'tonight'], ['Tomorrow morning', 'tomorrow'], ['This weekend', 'weekend'], ['Next week', 'next week']].map(([label, v]) => ({ label, run: () => act('snooze', v) })));
}
function moveMenu(anchor) {
  const acct = S.thread[0]?.account_id;
  const a = (S.ov?.accounts || []).find((x) => x.id === acct);
  if (!a) return;
  popMenu(anchor, (a.mailboxes || []).map((m) => ({ label: ROLE_LABEL[m.role] || m.name, run: () => act('move', m.name) })));
}

async function unsubscribe(m) {
  openDialog('Unsubscribe', (body, foot, close) => {
    body.append(h('p', { text: `Ask ${who(m.from)} to stop sending you mail?` }));
    foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn primary', text: 'Unsubscribe', onclick: async () => {
      try {
        const r = await api('/api/unsubscribe', { method: 'POST', body: { id: m.id } });
        close();
        if (r.done) toast(r.method === 'mailto' ? 'Unsubscribe email queued' : 'Unsubscribed'); else { window.open(r.url, '_blank', 'noopener,noreferrer'); toast('Opened the sender’s unsubscribe page'); }
      } catch (e) { fail(e); }
    } }));
  });
}

// ---------- dialogs ----------
function openDialog(title, build, opts = {}) {
  const dlg = $('#dlg'), inner = $('#dlg-inner');
  inner.replaceChildren();
  dlg.setAttribute('aria-labelledby', 'dlg-title');
  dlg.style.maxWidth = opts.wide ? 'min(880px, 96vw)' : '';
  const body = h('div', { class: 'dbody' }), foot = h('div', { class: 'dfoot' });
  inner.append(h('h2', { id: 'dlg-title', text: title }), body, foot);
  const close = () => dlg.close();
  build(body, foot, close);
  if (!dlg.open) dlg.showModal();
  const f = $('input:not([type=checkbox]), select, textarea', body); if (f && !opts.noFocus) f.focus();
  return close;
}

// ---------- compose ----------
const C = { draft: null, files: [], dirty: false, timer: null, busy: false, rich: false };

function parseRecipients(s) {
  const out = [];
  for (const part of s.split(/,(?=(?:[^"]*"[^"]*")*[^"]*$)/)) {
    const t = part.trim(); if (!t) continue;
    const m = /^(?:"?([^"<]*)"?\s*)?<([^>]+)>$/.exec(t);
    if (m) out.push({ name: (m[1] || '').trim(), addr: m[2].trim() }); else out.push({ name: '', addr: t.replace(/^mailto:/i, '') });
  }
  return out;
}

// ---- compose editor: plain text or formatted ----
function textToNodes(text) {
  const root = document.createElement('div');
  const urlRe = /(https?:\/\/[^\s<>"')]+[^\s<>"').,;:!?])/g;
  let quote = null;
  for (const line of String(text || '').replace(/\r\n/g, '\n').split('\n')) {
    const isQ = /^\s*>/.test(line);
    const parent = isQ ? (quote || (quote = root.appendChild(document.createElement('blockquote')))) : root;
    if (!isQ) quote = null;
    const content = isQ ? line.replace(/^\s*>+ ?/, '') : line;
    let last = 0, m;
    urlRe.lastIndex = 0;
    const div = document.createElement('div');
    while ((m = urlRe.exec(content))) { div.append(content.slice(last, m.index)); const a = document.createElement('a'); a.href = m[1]; a.textContent = m[1]; div.append(a); last = m.index + m[0].length; }
    div.append(content.slice(last));
    if (!content) div.append(document.createElement('br'));
    parent.append(div);
  }
  return root;
}
function setRich(on, keep) {
  C.rich = on;
  const ta = $('#c-body'), ed = $('#c-rich');
  if (on) { if (keep !== false) ed.replaceChildren(...textToNodes(ta.value).childNodes); ta.hidden = true; ed.hidden = false; $('#c-tools').hidden = false; }
  else { if (keep !== false) ta.value = ed.innerText.replace(/\n{3,}/g, '\n\n'); ed.hidden = true; ta.hidden = false; $('#c-tools').hidden = true; }
  $('#c-mode').setAttribute('aria-pressed', String(on));
  $('#c-mode').textContent = on ? 'Plain text' : 'Format';
}
const bodyField = () => (C.rich ? $('#c-rich') : $('#c-body'));
function insertLink() {
  const ed = $('#c-rich'), sel = getSelection();
  const range = sel.rangeCount ? sel.getRangeAt(0).cloneRange() : null;
  openDialog('Insert link', (body, foot, close) => {
    const url = h('input', { type: 'url', id: 'lk-url', placeholder: 'https://', autocomplete: 'off' });
    body.append(h('div', { class: 'form-row' }, h('label', { for: 'lk-url', text: 'Address' }), url));
    const apply = () => {
      const v = url.value.trim();
      close();
      if (!/^(https?:|mailto:)/i.test(v)) { toast('Links must start with https://, http:// or mailto:', { error: true }); return; }
      ed.focus();
      if (range) { sel.removeAllRanges(); sel.addRange(range); }
      if (sel.isCollapsed) document.execCommand('insertHTML', false, '<a href="' + v.replace(/"/g, '%22') + '">' + v.replace(/[<>&]/g, '') + '</a>'); else document.execCommand('createLink', false, v);
      scheduleSave();
    };
    url.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); apply(); } });
    foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn primary', text: 'Insert', onclick: apply }));
  });
}

function openCompose(draft = {}) {
  const dlg = $('#compose');
  C.draft = draft; C.files = (draft.files || []).map((f) => ({ ...f })); C.dirty = false;
  const from = $('#c-from'); from.replaceChildren();
  for (const a of S.ov?.accounts || []) if (a.enabled) from.append(h('option', { value: a.id, text: `${a.display_name || a.email} <${a.email}>` }));
  if (!from.options.length) { toast('Add an account first', { error: true }); return; }
  from.value = draft.account_id || S.settings['send.default_account'] || (guessAccount() || from.options[0].value);
  $('#c-to').value = addrList(draft.to); $('#c-cc').value = addrList(draft.cc); $('#c-bcc').value = addrList(draft.bcc);
  $('#c-ccrow').hidden = !(draft.cc && draft.cc.length); $('#c-bccrow').hidden = !(draft.bcc && draft.bcc.length);
  $('#c-subject').value = draft.subject || ''; $('#c-body').value = draft.body || '';
  const rich = draft.html_body ? true : ((draft._new || !draft.id) ? S.settings['ui.compose_rich'] !== 'false' : false);
  setRich(false, false); $('#c-rich').replaceChildren();
  if (rich) { if (draft.html_body) { const doc = new DOMParser().parseFromString(draft.html_body, 'text/html'); $('#c-rich').replaceChildren(...doc.body.childNodes); setRich(true, false); } else setRich(true); }
  $('#c-title').textContent = draft.subject ? draft.subject : 'New message';
  $('#c-status').textContent = draft.state === 'pending_approval' ? 'An agent asked to send this. Review it, then send or approve.' : '';
  renderFiles();
  if (!dlg.open) dlg.showModal();
  const first = draft.to && draft.to.length ? bodyField() : $('#c-to');
  first.focus();
  if (first.id === 'c-body') { first.setSelectionRange(0, 0); first.scrollTop = 0; } else if (first.id === 'c-rich') { first.scrollTop = 0; const r = document.createRange(); r.setStart(first, 0); r.collapse(true); const sl = getSelection(); sl.removeAllRanges(); sl.addRange(r); }
}
function guessAccount() {
  const k = S.view.key || '';
  const m = /^mb:(\d+)$/.exec(k);
  if (m) for (const a of S.ov?.accounts || []) if ((a.mailboxes || []).some((x) => String(x.id) === m[1])) return a.id;
  return null;
}
function composeTo(addr) { openCompose({ to: [{ name: '', addr }] }); }

function renderFiles() {
  const box = $('#c-files'); box.replaceChildren();
  C.files.forEach((f, i) => box.append(h('span', { class: 'att' }, `${f.filename} · ${fmtSize(f.size || 0)}`, h('button', { type: 'button', class: 'btn icon', 'aria-label': `Remove ${f.filename}`, text: '✕', onclick: () => { C.files.splice(i, 1); C.dirty = true; renderFiles(); scheduleSave(); } }))));
}

function collectDraft() {
  return {
    id: C.draft?.id || 0, account_id: $('#c-from').value,
    to: parseRecipients($('#c-to').value), cc: parseRecipients($('#c-cc').value), bcc: parseRecipients($('#c-bcc').value),
    subject: $('#c-subject').value, body: C.rich ? $('#c-rich').innerText : $('#c-body').value, html_body: C.rich ? $('#c-rich').innerHTML : '',
    in_reply_to: C.draft?.in_reply_to || '', references: C.draft?.references || [], reply_to_message: C.draft?.reply_to_message || 0,
    files: C.files.map((f) => ({ filename: f.filename, content_type: f.content_type, size: f.size, data_b64: f.data_b64 || '' })),
  };
}

async function saveDraft() {
  clearTimeout(C.timer);
  const d = collectDraft();
  const saved = await api('/api/drafts', { method: 'POST', body: d });
  C.draft = Object.assign({}, C.draft, saved); C.dirty = false;
  C.files.forEach((f) => { delete f.data_b64; });
  $('#c-status').textContent = 'Saved';
  return saved;
}
const scheduleSave = () => { C.dirty = true; clearTimeout(C.timer); C.timer = setTimeout(() => saveDraft().catch((e) => { $('#c-status').textContent = 'Not saved: ' + e.message; }), 2000); };

function composeEmpty() { return !$('#c-to').value.trim() && !$('#c-subject').value.trim() && !(C.rich ? $('#c-rich').innerText : $('#c-body').value).trim() && !C.files.length; }

async function closeCompose() {
  const dlg = $('#compose');
  if (C.busy) return;
  try {
    if (composeEmpty() && !C.draft?.id) { dlg.close(); return; }
    await saveDraft(); dlg.close(); toast('Draft saved'); loadDrafts();
  } catch (e) { fail(e); }
}

async function sendCompose() {
  if (C.busy) return;
  const d = collectDraft();
  if (!d.to.length && !d.cc.length && !d.bcc.length) { $('#c-status').textContent = 'Add a recipient.'; $('#c-to').focus(); return; }
  if (!d.subject.trim() && !confirmNoSubject()) { $('#c-subject').focus(); return; }
  C.busy = true; $('#c-send').disabled = true; $('#c-status').textContent = 'Sending…';
  try {
    const saved = await saveDraft();
    const q = await api(`/api/drafts/${saved.id}/send`, { method: 'POST' });
    $('#compose').close();
    const secs = Math.max(0, q.send_at - Math.floor(Date.now() / 1000));
    if (q.state === 'queued') {
      toast(secs > 0 ? `Sending in ${secs}s` : 'Sending…', { ms: Math.max(secs * 1000, 2500), action: secs > 0 ? { label: 'Undo', run: async () => { try { const back = await api(`/api/drafts/${saved.id}/cancel`, { method: 'POST' }); openCompose(await api('/api/drafts?id=' + back.id)); } catch (e) { fail(e); } } } : null });
    }
    announce('Message queued');
    loadDrafts(); refreshOverview();
  } catch (e) { $('#c-status').textContent = e.message; fail(e); }
  finally { C.busy = false; $('#c-send').disabled = false; }
}
function confirmNoSubject() { return window.confirm('Send without a subject?'); }

async function respond(kind) {
  if (!S.thread.length) return;
  const self = selfAddrs();
  const cands = S.thread.filter((m) => !self.has(m.from.addr.toLowerCase()));
  const target = (cands.length ? cands : S.thread)[(cands.length ? cands : S.thread).length - 1];
  try {
    const d = kind === 'forward' ? await api('/api/forward', { method: 'POST', body: { message_id: target.id } }) : await api('/api/reply', { method: 'POST', body: { message_id: target.id, all: kind === 'all' } });
    const full = await api('/api/drafts?id=' + d.id);
    full._new = true;
    openCompose(full);
  } catch (e) { fail(e); }
}

function wireCompose() {
  const dlg = $('#compose');
  for (const id of ['#c-to', '#c-cc', '#c-bcc', '#c-subject', '#c-body', '#c-from', '#c-rich']) $(id).addEventListener('input', scheduleSave);
  $('#c-mode').addEventListener('click', () => {
    if (C.rich && !confirm('Switch to plain text? Formatting will be removed.')) return;
    setRich(!C.rich); scheduleSave(); bodyField().focus();
  });
  $('#c-tools').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-cmd]'); if (!b) return;
    const cmd = b.dataset.cmd;
    $('#c-rich').focus();
    if (cmd === 'link') insertLink();
    else if (cmd.startsWith('formatBlock:')) document.execCommand('formatBlock', false, cmd.split(':')[1]);
    else document.execCommand(cmd, false);
    scheduleSave();
  });
  $('#c-rich').addEventListener('keydown', (e) => { if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); e.stopPropagation(); insertLink(); } });
  // paste as text: keeps pasted web page styling and scripts out of your message
  $('#c-rich').addEventListener('paste', (e) => { e.preventDefault(); const t = (e.clipboardData || window.clipboardData).getData('text/plain'); document.execCommand('insertText', false, t); });
  $('#c-ccbtn').addEventListener('click', () => { $('#c-ccrow').hidden = false; $('#c-bccrow').hidden = false; $('#c-cc').focus(); });
  $('#c-close').addEventListener('click', closeCompose);
  $('#c-send').addEventListener('click', sendCompose);
  $('#c-discard').addEventListener('click', async () => {
    try { if (C.draft?.id) await api('/api/drafts/' + C.draft.id, { method: 'DELETE' }); clearTimeout(C.timer); C.draft = null; dlg.close(); toast('Draft discarded'); loadDrafts(); } catch (e) { fail(e); }
  });
  dlg.addEventListener('cancel', (e) => { e.preventDefault(); closeCompose(); });
  dlg.addEventListener('keydown', (e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); sendCompose(); } });
  $('#c-attach').addEventListener('change', async (e) => {
    for (const f of e.target.files) {
      if (f.size > 25 * 1048576) { toast(`${f.name} is larger than 25 MB`, { error: true }); continue; }
      const b64 = await new Promise((res, rej) => { const r = new FileReader(); r.onload = () => res(String(r.result).split(',')[1] || ''); r.onerror = () => rej(r.error); r.readAsDataURL(f); });
      C.files.push({ filename: f.name, content_type: f.type || 'application/octet-stream', size: f.size, data_b64: b64 });
    }
    e.target.value = ''; renderFiles(); scheduleSave();
  });
  for (const k of ['to', 'cc', 'bcc']) suggest($('#c-' + k), $('#sug-' + k));
}

function suggest(input, ul) {
  let items = [], idx = -1;
  const hide = () => { ul.hidden = true; input.setAttribute('aria-expanded', 'false'); idx = -1; };
  const show = () => { ul.hidden = !items.length; input.setAttribute('aria-expanded', String(!!items.length)); };
  const mark = () => $$('li', ul).forEach((li, i) => li.setAttribute('aria-selected', String(i === idx)));
  const pick = (c) => { const parts = input.value.split(','); parts[parts.length - 1] = ' ' + addrText({ name: c.name, addr: c.addr }); input.value = parts.join(',').replace(/^\s+/, '') + ', '; hide(); input.focus(); scheduleSave(); };
  const query = debounce(async () => {
    const tok = input.value.split(',').pop().trim();
    if (tok.length < 1) { items = []; hide(); return; }
    try { items = await api('/api/contacts?q=' + encodeURIComponent(tok)); } catch (_) { items = []; }
    ul.replaceChildren(...items.map((c, i) => h('li', { role: 'option', id: ul.id + '-' + i, 'aria-selected': 'false', onmousedown: (e) => { e.preventDefault(); pick(c); } }, c.name ? `${c.name} ` : '', h('span', { style: { color: 'var(--fg-3)' }, text: c.name ? `<${c.addr}>` : c.addr }))));
    idx = -1; show();
  }, 120);
  input.addEventListener('input', query);
  input.addEventListener('blur', () => setTimeout(hide, 120));
  input.addEventListener('keydown', (e) => {
    if (ul.hidden) return;
    if (e.key === 'ArrowDown') { e.preventDefault(); idx = (idx + 1) % items.length; mark(); input.setAttribute('aria-activedescendant', ul.id + '-' + idx); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); idx = (idx - 1 + items.length) % items.length; mark(); input.setAttribute('aria-activedescendant', ul.id + '-' + idx); }
    else if ((e.key === 'Enter' || e.key === 'Tab') && idx >= 0) { e.preventDefault(); pick(items[idx]); }
    else if (e.key === 'Escape') { e.stopPropagation(); hide(); }
  });
}

// ---------- drafts page ----------
async function loadDrafts() { try { S.drafts = (await api('/api/drafts')) || []; renderSidebar(); if (S.page === 'drafts') renderPage(); } catch (_) { /* shown elsewhere */ } }

// ---------- pages ----------
function showPage(name) {
  S.page = name; S.view = { kind: 'page', key: 'page:' + name, title: name };
  $('#app').setAttribute('data-page', 'true'); closeMenu();
  renderSidebar(); renderPage();
  $('#page').focus && $('#page').setAttribute('tabindex', '-1');
}
async function renderPage() {
  const page = $('#page'); const y = page.scrollTop;
  try {
    const build = { drafts: pageDrafts, accounts: pageAccounts, settings: pageSettings, rules: pageRules, agents: pageAgents }[S.page];
    if (!build) return;
    const content = await build();
    page.replaceChildren(content);
    page.scrollTop = y;
  } catch (e) { page.replaceChildren(h('p', { text: e.message })); fail(e); }
}

function back() { return h('button', { class: 'btn', text: '← Mail', onclick: () => navigate({ kind: 'query', q: 'in:inbox', title: 'All Inboxes', key: 'smart:inbox' }), style: { marginBottom: '12px' } }); }

async function pageDrafts() {
  S.drafts = (await api('/api/drafts')) || [];
  const root = h('div', {}, back(), h('h2', { text: 'Drafts and outbox' }));
  if (!S.drafts.length) root.append(h('p', { class: 'empty', text: 'No drafts.' }));
  const states = { draft: 'Draft', pending_approval: 'Waiting for your approval', queued: 'Queued to send', sending: 'Sending', failed: 'Failed' };
  for (const d of S.drafts) {
    const card = h('div', { class: 'card' },
      h('div', {}, h('strong', { text: d.subject || '(no subject)' }), ' ', h('span', { class: 'pill' + (d.state === 'failed' || d.state === 'pending_approval' ? ' warn' : ''), text: states[d.state] || d.state }), d.author && d.author !== 'user' ? [' ', h('span', { class: 'pill', text: 'by ' + d.author })] : null),
      h('div', { style: { color: 'var(--fg-3)', fontSize: '.88rem' }, text: `To ${addrList(d.to) || '(nobody)'} · ${fmtDate(d.updated)}` }),
      d.error ? h('div', { style: { color: 'var(--danger)' }, text: d.error }) : null,
      h('div', { style: { whiteSpace: 'pre-wrap', margin: '8px 0', color: 'var(--fg-2)' }, text: (d.body || '').slice(0, 400) }));
    const row = h('div', { class: 'btnrow' });
    row.append(h('button', { class: 'btn', text: 'Open', onclick: async () => { try { openCompose(await api('/api/drafts?id=' + d.id)); } catch (e) { fail(e); } } }));
    if (d.state === 'pending_approval') row.append(h('button', { class: 'btn primary', text: 'Approve and send', onclick: async () => { try { await api(`/api/drafts/${d.id}/approve`, { method: 'POST' }); toast('Approved. Sending shortly.'); await loadDrafts(); refreshOverview(); } catch (e) { fail(e); } } }),
      h('button', { class: 'btn', text: 'Reject', onclick: async () => { try { await api(`/api/drafts/${d.id}/cancel`, { method: 'POST' }); toast('Returned to drafts'); await loadDrafts(); refreshOverview(); } catch (e) { fail(e); } } }));
    if (d.state === 'queued') row.append(h('button', { class: 'btn', text: 'Cancel send', onclick: async () => { try { await api(`/api/drafts/${d.id}/cancel`, { method: 'POST' }); toast('Send cancelled'); await loadDrafts(); } catch (e) { fail(e); } } }));
    if (d.state === 'failed') row.append(h('button', { class: 'btn primary', text: 'Retry', onclick: async () => { try { await api(`/api/drafts/${d.id}/send`, { method: 'POST' }); await loadDrafts(); } catch (e) { fail(e); } } }));
    row.append(h('button', { class: 'btn danger', text: 'Delete', onclick: async () => { try { await api('/api/drafts/' + d.id, { method: 'DELETE' }); await loadDrafts(); } catch (e) { fail(e); } } }));
    card.append(row); root.append(card);
  }
  return root;
}

// ---------- settings ----------
const DEFAULT_KEYS = {
  compose: 'c', search: '/', next: 'j', prev: 'k', open: 'Enter', reply: 'r', replyall: 'a', forward: 'f', archive: 'e', trash: '#',
  junk: '!', flag: 's', toggleread: 'u', snooze: 'z', refresh: 'R', palette: 'mod+k', help: '?',
  'go-inbox': 'g i', 'go-unread': 'g u', 'go-flagged': 'g f', 'go-sent': 'g s', 'go-drafts': 'g d', 'go-settings': 'g ,',
};
const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform);
const prettyKey = (k) => (k || '').replace(/mod\+/g, IS_MAC ? '⌘' : 'Ctrl+').replace(/shift\+/g, '⇧').replace(/alt\+/g, IS_MAC ? '⌥' : 'Alt+').replace(/(⌘|Ctrl\+|⇧|⌥|Alt\+)([a-z])$/, (m, a, b) => a + b.toUpperCase());
const KEY_LABEL = { compose: 'New message', search: 'Search', next: 'Next conversation', prev: 'Previous conversation', open: 'Open', reply: 'Reply', replyall: 'Reply all', forward: 'Forward', archive: 'Archive', trash: 'Move to Trash', junk: 'Junk', flag: 'Flag', toggleread: 'Read or unread', snooze: 'Snooze', refresh: 'Check for mail', palette: 'Command palette', help: 'Shortcut help', 'go-inbox': 'Go to Inbox', 'go-unread': 'Go to Unread', 'go-flagged': 'Go to Flagged', 'go-sent': 'Go to Sent', 'go-drafts': 'Go to Drafts', 'go-settings': 'Go to Settings' };

function control(item) {
  const v = S.settings[item.k] ?? '';
  const save = async (val) => { try { if (item.k === 'ui.notifications' && val === true && 'Notification' in window && Notification.permission === 'default') { const r = await Notification.requestPermission(); if (r !== 'granted') { toast('Notifications are blocked in the browser', { error: true }); renderPage(); return; } } await putSetting(item.k, val); announce(`${item.label} saved`); } catch (e) { fail(e); renderPage(); } };
  let el;
  if (item.type === 'select') el = h('select', { id: 'set-' + item.k, onchange: (e) => save(e.target.value) }, item.opts.map(([val, label]) => h('option', { value: val, text: label, selected: val === v })));
  else if (item.type === 'bool') el = h('input', { type: 'checkbox', id: 'set-' + item.k, checked: v === 'true', onchange: (e) => save(e.target.checked) });
  else if (item.type === 'range') { const out = h('output', { for: 'set-' + item.k, text: Math.round(parseFloat(v || '1') * 100) + '%', style: { marginLeft: '10px' } }); const r = h('input', { type: 'range', id: 'set-' + item.k, min: item.min, max: item.max, step: item.step, value: v, oninput: (e) => { out.textContent = Math.round(e.target.value * 100) + '%'; }, onchange: (e) => save(e.target.value) }); el = h('div', { style: { display: 'flex', alignItems: 'center', maxWidth: '560px' } }, r, out); }
  else if (item.type === 'color') el = h('input', { type: 'color', id: 'set-' + item.k, value: v || '#0a84ff', onchange: (e) => save(e.target.value) });
  else if (item.type === 'secret') el = h('input', { type: 'password', id: 'set-' + item.k, placeholder: v ? 'set' : '', autocomplete: 'off', onchange: (e) => e.target.value && save(e.target.value) });
  else el = h('input', { type: item.type === 'number' ? 'number' : 'text', id: 'set-' + item.k, value: v, min: item.type === 'number' ? 0 : false, onchange: (e) => save(e.target.value) });
  return h('div', { class: 'form-row' }, h('label', { for: 'set-' + item.k, text: item.label }), el, item.hint ? h('div', { class: 'hint', text: item.hint }) : null);
}

async function pageSettings() {
  S.settings = await api('/api/settings');
  const accts = (S.ov?.accounts || []).map((a) => [a.id, a.email]);
  const schema = [
    ['Appearance', [
      { k: 'ui.theme', label: 'Theme', type: 'select', opts: [['system', 'Match system'], ['light', 'Light'], ['dark', 'Dark'], ['high-contrast', 'High contrast']] },
      { k: 'ui.density', label: 'Density', type: 'select', opts: [['compact', 'Compact'], ['comfortable', 'Comfortable'], ['spacious', 'Spacious']] },
      { k: 'ui.layout', label: 'Layout', type: 'select', opts: [['three-pane', 'Three columns'], ['two-pane', 'Two columns'], ['list-only', 'List only']] },
      { k: 'ui.font_scale', label: 'Text size', type: 'range', min: 0.8, max: 1.8, step: 0.05 },
      { k: 'ui.accent', label: 'Accent colour', type: 'color' },
      { k: 'ui.preview_lines', label: 'Preview lines', type: 'number' },
      { k: 'ui.dark_mail', label: 'Dark message bodies', type: 'bool', hint: 'Invert white emails in dark themes.' },
      { k: 'ui.threads', label: 'Group into conversations', type: 'bool' },
      { k: 'ui.notifications', label: 'Desktop notifications for new mail', type: 'bool' },
      { k: 'ui.reduce_motion', label: 'Motion', type: 'select', opts: [['system', 'Match system'], ['reduce', 'Reduce']] },
    ]],
    ['Reading', [
      { k: 'ui.mark_read_seconds', label: 'Mark read after (seconds)', type: 'number' },
      { k: 'privacy.remote_content', label: 'Remote images', type: 'select', opts: [['block', 'Block until I allow them'], ['allow', 'Always load']], },
    ]],
    ['Sending', [
      { k: 'send.undo_seconds', label: 'Undo send window (seconds)', type: 'number' },
      { k: 'send.default_account', label: 'Default account', type: 'select', opts: [['', 'Automatic'], ...accts] },
      { k: 'ui.compose_rich', label: 'Compose with formatting', type: 'bool' },
    ]],
    ['Sync', [
      { k: 'sync.days', label: 'First sync reaches back (days)', type: 'number', hint: '0 = everything' },
      { k: 'sync.poll_seconds', label: 'Full refresh every (seconds)', type: 'number' },
    ]],
    ['Agents', [
      { k: 'agent.send_policy', label: 'When an agent sends', type: 'select', opts: [['draft_only', 'Never: drafts only'], ['approve', 'Hold for my approval'], ['allow', 'Send with undo window']] },
      { k: 'agent.expose_bodies', label: 'Agents can read message bodies', type: 'bool' },
      { k: 'agent.allowed_domains', label: 'Allowed recipient domains', type: 'text', hint: 'Comma separated. Empty means any domain.' },
      { k: 'agent.max_sends_hour', label: 'Agent sends per hour', type: 'number' },
    ]],
    ['Google and Microsoft sign-in', [
      { k: 'oauth.google.client_id', label: 'Google client ID', type: 'text' },
      { k: 'oauth.google.client_secret', label: 'Google client secret', type: 'secret' },
      { k: 'oauth.microsoft.client_id', label: 'Microsoft client ID', type: 'text', hint: 'Without these, Gmail and Outlook use app passwords.' },
    ]],
  ];
  const root = h('div', {}, back(), h('h2', { text: 'Settings' }));
  for (const [sec, items] of schema) root.append(h('h3', { text: sec }), h('div', { class: 'card' }, items.map(control)));
  // keyboard
  root.append(h('h3', { text: 'Keyboard shortcuts' }));
  const tbl = h('table', { class: 'data' }, h('thead', {}, h('tr', {}, h('th', { text: 'Action' }), h('th', { text: 'Keys' }))));
  const tb = h('tbody'); tbl.append(tb);
  let custom = {}; try { custom = S.settings['ui.keybindings'] ? JSON.parse(S.settings['ui.keybindings']) : {}; } catch (_) { /* invalid */ }
  for (const [k, label] of Object.entries(KEY_LABEL)) {
    const inp = h('input', { type: 'text', 'aria-label': label + ' shortcut', value: S.keys[k] || '', style: { maxWidth: '12rem' }, onchange: async (e) => {
      const next = Object.assign({}, custom); const val = e.target.value.trim();
      if (!val || val === DEFAULT_KEYS[k]) delete next[k]; else next[k] = val;
      try { await putSetting('ui.keybindings', Object.keys(next).length ? JSON.stringify(next) : ''); custom = next; announce('Shortcut saved'); } catch (er) { fail(er); }
    } });
    tb.append(h('tr', {}, h('td', { text: label }), h('td', {}, inp)));
  }
  root.append(h('div', { class: 'card tablewrap' }, tbl, h('p', { class: 'hint', style: { color: 'var(--fg-3)' }, text: 'Use "mod+k" for ⌘/Ctrl, and "g i" for a two-key sequence. Clear a field to restore its default.' }),
    h('div', { class: 'btnrow' }, h('button', { class: 'btn', text: 'Reset all shortcuts', onclick: async () => { try { await putSetting('ui.keybindings', ''); renderPage(); } catch (e) { fail(e); } } }))));
  return root;
}

// ---------- accounts ----------
async function pageAccounts() {
  await refreshOverview(true);
  const root = h('div', {}, back(), h('h2', { text: 'Accounts' }),
    h('div', { class: 'btnrow', style: { marginBottom: '16px' } },
      h('button', { class: 'btn primary', text: 'Add account', onclick: () => addAccountDialog() }),
      h('button', { class: 'btn', text: 'Import from Apple Mail', onclick: () => importAppleDialog() })));
  const st = Object.fromEntries((S.ov.status || []).map((s) => [s.account, s]));
  if (!S.ov.accounts.length) root.append(h('p', { class: 'empty', text: 'No accounts yet.' }));
  for (const a of S.ov.accounts) {
    const s = st[a.id];
    const card = h('div', { class: 'card' },
      h('div', {}, h('span', { class: 'acct', style: { display: 'inline-block', width: '10px', height: '10px', borderRadius: '50%', background: a.color, marginRight: '8px' } }), h('strong', { text: a.email }), ' ',
        h('span', { class: 'pill', text: a.enabled ? (s ? s.state : 'idle') : 'off' }), ' ', h('span', { class: 'pill', text: a.auth === 'oauth2' ? 'OAuth' : 'password' }), a.source === 'apple' ? [' ', h('span', { class: 'pill', text: 'from Apple Mail' })] : null),
      h('div', { style: { color: 'var(--fg-3)', fontSize: '.86rem', margin: '4px 0' }, text: `IMAP ${a.imap_host}:${a.imap_port} · SMTP ${a.smtp_host}:${a.smtp_port}` }),
      s?.error ? h('div', { style: { color: 'var(--danger)' }, role: 'alert', text: s.error }) : null);
    const row = h('div', { class: 'btnrow' });
    row.append(
      h('button', { class: 'btn', text: 'Edit', onclick: () => editAccountDialog(a) }),
      h('button', { class: 'btn', text: 'Check sign-in', onclick: async (e) => { e.target.disabled = true; try { await api(`/api/accounts/${encodeURIComponent(a.id)}/test`, { method: 'POST' }); toast('Signed in to IMAP and SMTP'); } catch (er) { fail(er); } e.target.disabled = false; } }),
      h('button', { class: 'btn', text: a.auth === 'oauth2' ? 'Sign in again' : 'Change password', onclick: () => credentialDialog(a) }),
      h('button', { class: 'btn', text: 'Folders', onclick: () => foldersDialog(a) }),
      h('button', { class: 'btn', text: a.enabled ? 'Turn off' : 'Turn on', onclick: async () => { try { await api('/api/accounts/' + encodeURIComponent(a.id), { method: 'PATCH', body: { enabled: !a.enabled } }); await refreshOverview(); renderPage(); } catch (e) { fail(e); } } }),
      h('button', { class: 'btn danger', text: 'Remove', onclick: () => removeAccountDialog(a) }));
    card.append(row); root.append(card);
  }
  return root;
}

function editAccountDialog(a) {
  openDialog('Edit ' + a.email, (body, foot, close) => {
    const name = h('input', { type: 'text', id: 'ea-name', value: a.display_name || '' });
    const sig = h('textarea', { id: 'ea-sig', rows: 5 }); sig.value = a.signature || '';
    const color = h('input', { type: 'color', id: 'ea-color', value: a.color || '#0a84ff' });
    const days = h('input', { type: 'number', id: 'ea-days', min: 0, value: a.sync_days || 0 });
    body.append(
      h('div', { class: 'form-row' }, h('label', { for: 'ea-name', text: 'Name on outgoing mail' }), name),
      h('div', { class: 'form-row' }, h('label', { for: 'ea-sig', text: 'Signature' }), sig),
      h('div', { class: 'form-row' }, h('label', { for: 'ea-color', text: 'Colour' }), color),
      h('div', { class: 'form-row' }, h('label', { for: 'ea-days', text: 'Keep (days)' }), days, h('div', { class: 'hint', text: '0 follows the global sync setting.' })));
    foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn primary', text: 'Save', onclick: async () => {
      try { await api('/api/accounts/' + encodeURIComponent(a.id), { method: 'PATCH', body: { display_name: name.value, signature: sig.value, color: color.value, sync_days: parseInt(days.value || '0', 10) } }); close(); await refreshOverview(); renderPage(); toast('Saved'); } catch (e) { fail(e); }
    } }));
  });
}

function credentialDialog(a) {
  if (a.auth === 'oauth2') {
    openDialog('Sign in again', (body, foot, close) => {
      body.append(h('p', { text: `Sign in to ${a.email} in your browser.` }));
      foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn primary', text: 'Open browser', onclick: async () => { try { const r = await api(`/api/accounts/${encodeURIComponent(a.id)}/login`, { method: 'POST' }); window.open(r.auth_url, '_blank', 'noopener'); close(); toast('Finish signing in in the new tab'); } catch (e) { fail(e); } } }));
    });
    return;
  }
  openDialog('Change password', (body, foot, close) => {
    const pw = h('input', { type: 'password', id: 'cp-pw', autocomplete: 'off' });
    body.append(h('div', { class: 'form-row' }, h('label', { for: 'cp-pw', text: 'New password' }), pw, h('div', { class: 'hint', text: 'Use an app password if your provider offers one.' })));
    foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn primary', text: 'Save', onclick: async (e) => { e.target.disabled = true; try { await api(`/api/accounts/${encodeURIComponent(a.id)}/password`, { method: 'POST', body: { password: pw.value } }); close(); toast('Password updated'); refreshOverview(); } catch (er) { fail(er); e.target.disabled = false; } } }));
  });
}

function foldersDialog(a) {
  openDialog('Folders to sync', (body, foot, close) => {
    body.append(h('p', { class: 'hint', style: { color: 'var(--fg-3)' }, text: 'Unchecked folders are not downloaded.' }));
    for (const m of a.mailboxes || []) {
      body.append(h('div', { class: 'form-row' }, h('label', { for: 'f-' + m.id, text: m.name + (m.role ? ` (${m.role})` : '') }),
        h('input', { type: 'checkbox', id: 'f-' + m.id, checked: m.subscribed, onchange: async (e) => { try { await api('/api/mailboxes/sync', { method: 'POST', body: { account: a.id, mailbox: m.name, on: e.target.checked } }); m.subscribed = e.target.checked; refreshOverview(); } catch (er) { fail(er); e.target.checked = !e.target.checked; } } })));
    }
    foot.append(h('button', { class: 'btn primary', text: 'Done', onclick: close }));
  });
}

function removeAccountDialog(a) {
  openDialog('Remove account', (body, foot, close) => {
    body.append(h('p', { text: `Remove ${a.email} from OpenMail? Its local copy of mail and saved password are deleted. Mail on the server is not touched.` }));
    foot.append(h('button', { class: 'btn', text: 'Cancel', onclick: close }), h('button', { class: 'btn danger', text: 'Remove', onclick: async () => { try { await api('/api/accounts/' + encodeURIComponent(a.id), { method: 'DELETE' }); close(); await refreshOverview(); renderPage(); loadList(); toast(a.email + ' removed'); } catch (e) { fail(e); } } }));
  });
}

function addAccountDialog() {
  openDialog('Add account', (body, foot, close) => {
    const email = h('input', { type: 'email', id: 'aa-email', autocomplete: 'email', placeholder: 'you@example.com' });
    const name = h('input', { type: 'text', id: 'aa-name', autocomplete: 'name' });
    const found = h('div', { 'aria-live': 'polite' });
    const pw = h('input', { type: 'password', id: 'aa-pw', autocomplete: 'off' });
    const adv = h('div', { hidden: true });
    const fields = {};
    const advRow = (key, label, val, type = 'text') => { fields[key] = h('input', { type, id: 'aa-' + key, value: val ?? '' }); return h('div', { class: 'form-row' }, h('label', { for: 'aa-' + key, text: label }), fields[key]); };
    adv.append(advRow('username', 'Username', ''), advRow('imap_host', 'IMAP server', ''), advRow('imap_port', 'IMAP port', 993, 'number'), advRow('imap_security', 'IMAP security (tls or starttls)', 'tls'),
      advRow('smtp_host', 'SMTP server', ''), advRow('smtp_port', 'SMTP port', 465, 'number'), advRow('smtp_security', 'SMTP security (tls or starttls)', 'tls'));
    const status = h('div', { role: 'status', style: { color: 'var(--fg-3)', minHeight: '1.4em' } });
    let disc = null;
    const advBtn = h('button', { class: 'btn', type: 'button', text: 'Enter server settings myself', onclick: () => { adv.hidden = !adv.hidden; } });
    const go = h('button', { class: 'btn primary', text: 'Continue' });
    const run = async () => {
      go.disabled = true; status.textContent = disc ? 'Checking sign-in…' : 'Looking up your mail servers…';
      try {
        if (!disc) {
          try {
            disc = await api('/api/discover?email=' + encodeURIComponent(email.value.trim()));
            found.replaceChildren(h('div', { class: 'card' }, h('strong', { text: disc.provider || 'Mail servers found' }), h('div', { style: { color: 'var(--fg-3)', fontSize: '.88rem' }, text: `${disc.imap_host} · ${disc.smtp_host}` }), disc.help ? h('p', { text: disc.help }, ' ', disc.help_url ? h('a', { href: disc.help_url, target: '_blank', rel: 'noopener', text: 'Learn more' }) : null) : null));
            fields.username.value = disc.username || email.value.trim();
            Object.assign(fields.imap_host, { value: disc.imap_host }); fields.imap_port.value = disc.imap_port; fields.imap_security.value = disc.imap_security;
            fields.smtp_host.value = disc.smtp_host; fields.smtp_port.value = disc.smtp_port; fields.smtp_security.value = disc.smtp_security;
            if (disc.auth === 'oauth2') { go.textContent = 'Sign in with ' + (disc.oauth_provider === 'microsoft' ? 'Microsoft' : 'Google'); pw.closest('.form-row').hidden = true; } else { go.textContent = 'Add account'; pw.closest('.form-row').hidden = false; pw.focus(); }
            email.readOnly = true; status.textContent = '';
          } catch (e) { adv.hidden = false; disc = { manual: true }; found.replaceChildren(h('p', { role: 'alert', text: e.message })); status.textContent = 'Enter the server settings below.'; go.textContent = 'Add account'; }
          go.disabled = false; return;
        }
        const req = { email: email.value.trim(), display_name: name.value.trim(), password: pw.value, username: fields.username.value.trim() };
        if (!adv.hidden || disc.manual) Object.assign(req, { imap_host: fields.imap_host.value.trim(), imap_port: +fields.imap_port.value, imap_security: fields.imap_security.value.trim(), smtp_host: fields.smtp_host.value.trim(), smtp_port: +fields.smtp_port.value, smtp_security: fields.smtp_security.value.trim() });
        const res = await api('/api/accounts', { method: 'POST', body: req });
        if (res.needs_oauth) {
          const r = await api(`/api/accounts/${encodeURIComponent(res.account.id)}/login`, { method: 'POST' });
          window.open(r.auth_url, '_blank', 'noopener');
          status.textContent = 'Finish signing in in the new browser tab…';
          const wait = setInterval(async () => { await refreshOverview(true); const st = (S.ov.status || []).find((x) => x.account === res.account.id); if (st && (st.last_sync || st.state === 'syncing')) { clearInterval(wait); close(); toast(res.account.email + ' added'); } }, 2000);
          return;
        }
        close(); toast(req.email + ' added. Downloading mail…'); await refreshOverview(); if (S.page === 'accounts') renderPage(); loadList();
      } catch (e) { status.textContent = ''; found.append(h('p', { role: 'alert', style: { color: 'var(--danger)' }, text: e.message })); fail(e); }
      go.disabled = false;
    };
    go.addEventListener('click', run);
    body.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target.tagName === 'INPUT') { e.preventDefault(); run(); } });
    body.append(h('div', { class: 'form-row' }, h('label', { for: 'aa-email', text: 'Email address' }), email), h('div', { class: 'form-row' }, h('label', { for: 'aa-name', text: 'Your name' }), name), found,
      h('div', { class: 'form-row' }, h('label', { for: 'aa-pw', text: 'Password' }), pw), adv, status);
    pw.closest('.form-row').hidden = true;
    foot.append(advBtn, /Mac/.test(navigator.platform) ? h('button', { class: 'btn', type: 'button', text: 'Import from Apple Mail', onclick: () => { close(); setTimeout(importAppleDialog, 50); } }) : null, h('span', { class: 'spacer' }), h('button', { class: 'btn', text: 'Cancel', onclick: close }), go);
  });
}

async function importAppleDialog() {
  openDialog('Import from Apple Mail', async (body, foot, close) => {
    const status = h('p', { role: 'status', text: 'Reading Apple Mail’s settings…' });
    body.append(status);
    foot.append(h('button', { class: 'btn', text: 'Close', onclick: close }));
    let found;
    const scan = async (from) => {
      try { found = await api('/api/import/apple' + (from ? '?from=' + encodeURIComponent(from) : '')); render(); }
      catch (e) {
        status.replaceChildren(h('span', { role: 'alert', text: e.message }));
        const p = h('input', { type: 'text', id: 'ia-from', placeholder: '/path/to/copy-of-home' });
        body.append(h('div', { class: 'form-row' }, h('label', { for: 'ia-from', text: 'Folder with a copy' }), p, h('div', { class: 'hint', text: 'A copy of ~/Library/Accounts and ~/Library/Mail inside a home-shaped folder.' })), h('div', { class: 'btnrow' }, h('button', { class: 'btn', text: 'Try that folder', onclick: () => scan(p.value.trim()) })));
      }
    };
    const render = () => {
      body.replaceChildren();
      body.append(h('p', { text: `${found.accounts.length} account(s), ${found.rules.length} rule(s), ${found.vips.length} VIP(s) found.` }));
      const pws = {}, picks = {};
      const kc = h('input', { type: 'checkbox', id: 'ia-kc', checked: true });
      body.append(h('div', { class: 'form-row' }, h('label', { for: 'ia-kc', text: 'Look for saved passwords in the macOS Keychain first' }), kc, h('div', { class: 'hint', text: 'macOS asks you to allow each one.' })));
      for (const c of found.accounts) {
        const cb = h('input', { type: 'checkbox', id: 'ia-' + c.email, checked: true }); picks[c.email] = cb;
        const pw = h('input', { type: 'password', id: 'ia-pw-' + c.email, autocomplete: 'off', placeholder: 'App password (if the Keychain has none)' }); pws[c.email] = pw;
        body.append(h('div', { class: 'card' }, h('div', {}, cb, ' ', h('label', { for: 'ia-' + c.email }, h('strong', { text: c.email }), ' ', h('span', { class: 'pill', text: c.kind }))), c.note ? h('div', { class: 'hint', style: { color: 'var(--fg-3)', fontSize: '.84rem' }, text: c.note }) : null, h('div', { style: { marginTop: '6px' } }, pw)));
      }
      for (const w of found.warnings || []) body.append(h('p', { style: { color: 'var(--fg-3)', fontSize: '.85rem' }, text: 'Note: ' + w }));
      const out = h('div');
      body.append(out);
      foot.replaceChildren(h('button', { class: 'btn', text: 'Close', onclick: close }), h('button', { class: 'btn primary', text: 'Import', onclick: async (e) => {
        e.target.disabled = true; out.replaceChildren(h('p', { role: 'status', text: 'Importing and checking each sign-in…' }));
        try {
          const only = found.accounts.filter((c) => picks[c.email].checked).map((c) => c.email);
          const passwords = {}; for (const c of found.accounts) if (pws[c.email].value) passwords[c.email] = pws[c.email].value;
          const rep = await api('/api/import/apple', { method: 'POST', body: { only, passwords, try_keychain: kc.checked } });
          out.replaceChildren(h('table', { class: 'data' }, h('tbody', {}, rep.results.map((r) => h('tr', {}, h('td', { text: r.email }), h('td', { text: r.status.replace('_', ' ') }), h('td', { text: r.detail || '' }))))), h('p', { text: `${rep.rules_imported} rule(s), ${rep.signatures_imported} signature(s), ${rep.vips_imported} VIP(s) imported.` }));
          await refreshOverview(); if (S.page === 'accounts') renderPage();
        } catch (er) { out.replaceChildren(h('p', { role: 'alert', text: er.message })); }
        e.target.disabled = false;
      } }));
    };
    scan('');
  }, { wide: true, noFocus: true });
}

// ---------- rules ----------
const FIELD_LABEL = { from: 'From', to: 'To', cc: 'Cc', subject: 'Subject', body: 'Body', list_id: 'Mailing list', any_recipient: 'Any recipient', has_attachment: 'Has attachment' };
const OP_LABEL = { contains: 'contains', not_contains: 'does not contain', equals: 'is', begins: 'starts with', ends: 'ends with', regex: 'matches', exists: 'is present' };
const ACT_LABEL = { move: 'move to', copy: 'copy to', archive: 'archive', mark_read: 'mark as read', mark_unread: 'mark as unread', flag: 'flag', unflag: 'unflag', keyword: 'tag', delete: 'move to Trash' };
const ruleCond = (c) => `${FIELD_LABEL[c.field] || c.field} ${OP_LABEL[c.op] || c.op}${c.op === 'exists' ? '' : ` “${c.value}”`}`;
const ruleAct = (a) => `${ACT_LABEL[a.kind] || a.kind}${a.value ? ' ' + a.value : ''}`;

async function pageRules() {
  const rules = await api('/api/rules');
  const root = h('div', {}, back(), h('h2', { text: 'Rules' }));
  if (!rules.length) root.append(h('p', { class: 'empty', text: 'No rules.' }));
  for (const r of rules) {
    root.append(h('div', { class: 'card' },
      h('div', {}, h('strong', { text: r.name }), ' ', h('span', { class: 'pill' + (r.enabled ? '' : ' warn'), text: r.enabled ? 'on' : 'off' }), r.source ? [' ', h('span', { class: 'pill', text: r.source })] : null),
      h('div', { style: { color: 'var(--fg-2)', margin: '4px 0' }, text: `If ${r.conditions.map(ruleCond).join(r.match_all ? ' and ' : ' or ')}: ${r.actions.map(ruleAct).join(', ')}` }),
      h('div', { class: 'btnrow' },
        h('button', { class: 'btn', text: r.enabled ? 'Turn off' : 'Turn on', onclick: async () => { try { await api('/api/rules', { method: 'POST', body: { ...r, enabled: !r.enabled } }); renderPage(); } catch (e) { fail(e); } } }),
        h('button', { class: 'btn danger', text: 'Delete', onclick: async () => { try { await api('/api/rules/' + r.id, { method: 'DELETE' }); renderPage(); } catch (e) { fail(e); } } }))));
  }
  if (rules.length) root.append(h('div', { class: 'btnrow' },
    h('button', { class: 'btn', text: 'Test on the last 30 days of Inbox', onclick: async () => { try { const r = await api('/api/rules/run', { method: 'POST', body: { query: 'in:inbox after:30d', dry_run: true } }); toast(`${r.matched} message(s) would be changed`); } catch (e) { fail(e); } } }),
    h('button', { class: 'btn', text: 'Apply to the last 30 days of Inbox', onclick: async () => { try { const r = await api('/api/rules/run', { method: 'POST', body: { query: 'in:inbox after:30d' } }); toast(`${r.matched} message(s) changed`); loadList(); } catch (e) { fail(e); } } })));
  const folders = [...new Set((S.ov?.accounts || []).flatMap((a) => (a.mailboxes || []).map((m) => m.name)))];
  const dl = h('datalist', { id: 'folder-list' }, folders.map((f) => h('option', { value: f })));
  const folderRow = h('div', { class: 'form-row', hidden: true }, h('label', { for: 'r-folder', text: 'Folder' }), h('input', { type: 'text', id: 'r-folder', name: 'folder', list: 'folder-list', autocomplete: 'off' }));
  const action = h('select', { id: 'r-action', name: 'action', onchange: () => { folderRow.hidden = action.value !== 'move'; } }, [['archive', 'Archive'], ['mark_read', 'Mark as read'], ['flag', 'Flag'], ['move', 'Move to folder…']].map(([v, l]) => h('option', { value: v, text: l })));
  const form = h('form', { class: 'card', onsubmit: async (e) => {
    e.preventDefault();
    const v = (n) => form.elements[n].value.trim();
    const kind = v('action');
    if (kind === 'move' && !v('folder')) { toast('Choose a folder to move to', { error: true }); return; }
    const rule = { name: v('name') || `${FIELD_LABEL[v('field')]} ${v('value')}`, enabled: true, match_all: true, conditions: [{ field: v('field'), op: 'contains', value: v('value') }], actions: [kind === 'move' ? { kind: 'move', value: v('folder') } : { kind }] };
    try { await api('/api/rules', { method: 'POST', body: rule }); toast('Rule saved'); renderPage(); } catch (er) { fail(er); }
  } },
    h('h3', { text: 'New rule', style: { marginTop: 0 } }),
    h('div', { class: 'form-row' }, h('label', { for: 'r-name', text: 'Name' }), h('input', { type: 'text', id: 'r-name', name: 'name', placeholder: 'Optional' })),
    h('div', { class: 'form-row' }, h('label', { for: 'r-field', text: 'If' }), h('select', { id: 'r-field', name: 'field' }, [['from', 'From'], ['to', 'To'], ['subject', 'Subject'], ['list_id', 'Mailing list'], ['body', 'Body']].map(([v, l]) => h('option', { value: v, text: l + ' contains' })))),
    h('div', { class: 'form-row' }, h('label', { for: 'r-value', text: 'Text' }), h('input', { type: 'text', id: 'r-value', name: 'value', required: true })),
    h('div', { class: 'form-row' }, h('label', { for: 'r-action', text: 'Then' }), action),
    folderRow, dl,
    h('div', { class: 'btnrow' }, h('button', { class: 'btn primary', type: 'submit', text: 'Save rule' })));
  root.append(form);
  return root;
}

// ---------- agents ----------
async function pageAgents() {
  const [tokens, audit, drafts] = await Promise.all([api('/api/tokens'), api('/api/audit?limit=60'), api('/api/drafts')]);
  S.drafts = drafts || [];
  const root = h('div', {}, back(), h('h2', { text: 'Agents' }));
  const pending = drafts.filter((d) => d.state === 'pending_approval');
  root.append(h('div', { class: 'card' }, h('strong', { text: `Send policy: ${({ draft_only: 'agents only write drafts', approve: 'agents ask for your approval', allow: 'agents send with an undo window' })[S.settings['agent.send_policy']] || S.settings['agent.send_policy']}` }),
    h('div', { class: 'btnrow' }, h('button', { class: 'btn', text: 'Change in Settings', onclick: () => showPage('settings') }))));
  if (pending.length) { root.append(h('h3', { text: 'Waiting for your approval' })); root.append(h('p', {}, h('button', { class: 'btn primary', text: `Review ${pending.length} draft(s)`, onclick: () => showPage('drafts') }))); }

  root.append(h('h3', { text: 'Connect an agent' }));
  const snippet = (label, text) => h('div', {}, h('div', { class: 'lbl', text: label }), h('pre', { class: 'code', text }), h('button', { class: 'btn', text: 'Copy', onclick: () => navigator.clipboard.writeText(text).then(() => toast('Copied'), () => toast('Copy failed: select the text instead', { error: true })) }));
  const base = location.origin;
  root.append(h('div', { class: 'card' },
    snippet('Claude Code (stdio, can read, draft and organise)', 'claude mcp add openmail -- openmail mcp --scope write'),
    snippet('Claude Desktop, Cursor and others (JSON)', JSON.stringify({ mcpServers: { openmail: { command: 'openmail', args: ['mcp', '--scope', 'write'] } } }, null, 2)),
    snippet('Over HTTP (create a key below first)', `claude mcp add --transport http openmail ${base}/mcp --header "Authorization: Bearer <key>"`),
    h('p', { style: { color: 'var(--fg-3)', fontSize: '.86rem' }, text: 'Scopes: read (search and read), write (also drafts and organising), send (also sending, subject to the send policy), admin (also rules, settings, accounts).' })));

  root.append(h('h3', { text: 'Keys' }));
  const created = h('div');
  const name = h('input', { type: 'text', id: 'tk-name', placeholder: 'Name, e.g. claude-desktop', style: { maxWidth: '16rem' } });
  const scope = h('select', { id: 'tk-scope', 'aria-label': 'Scope' }, ['read', 'write', 'send', 'admin'].map((s) => h('option', { value: s, text: s, selected: s === 'write' })));
  const tcard = h('div', { class: 'card' }, h('div', { class: 'btnrow', style: { marginTop: 0 } }, h('label', { class: 'sr-only', for: 'tk-name', text: 'Key name' }), name, scope, h('button', { class: 'btn primary', text: 'Create key', onclick: async () => {
    try { const r = await api('/api/tokens', { method: 'POST', body: { name: name.value.trim(), scope: scope.value } }); created.replaceChildren(h('div', { class: 'banner info', role: 'status' }, h('div', {}, 'Copy this key now. It is not shown again.', h('pre', { class: 'code', text: r.token })))); name.value = ''; const t2 = await api('/api/tokens'); renderTokens(t2); } catch (e) { fail(e); }
  } })), created);
  const tl = h('div'); tcard.append(tl); root.append(tcard);
  const renderTokens = (list) => list.filter((t) => t.name !== 'web-ui').length === 0 ? tl.replaceChildren(h('p', { style: { color: 'var(--fg-3)' }, text: 'No keys.' })) : tl.replaceChildren(h('table', { class: 'data' }, h('thead', {}, h('tr', {}, ['Name', 'Scope', 'Last used', ''].map((x) => h('th', { text: x })))), h('tbody', {}, list.filter((t) => t.name !== 'web-ui').map((t) => h('tr', {}, h('td', { text: t.name }), h('td', { text: t.scope }), h('td', { text: t.last_used ? fmtDate(t.last_used, true) : 'never' }), h('td', {}, h('button', { class: 'btn danger', text: 'Revoke', onclick: async () => { try { await api('/api/tokens/' + encodeURIComponent(t.name), { method: 'DELETE' }); renderTokens(await api('/api/tokens')); } catch (e) { fail(e); } } })))))));
  renderTokens(tokens);

  root.append(h('h3', { text: 'Activity' }));
  root.append(h('div', { class: 'card tablewrap' }, h('table', { class: 'data' }, h('thead', {}, h('tr', {}, ['When', 'Who', 'Action', 'Details', ''].map((x) => h('th', { text: x })))),
    h('tbody', {}, audit.map((e) => h('tr', {}, h('td', { text: fmtDate(e.ts, true) }), h('td', { text: e.actor }), h('td', { text: e.action }), h('td', { class: 'mono', text: (e.args || '').slice(0, 140) }), h('td', { text: e.ok ? '' : '✕ ' + e.error, style: { color: 'var(--danger)' } })))))));
  return root;
}

// ---------- palette & help ----------
function commands() {
  const c = [
    { label: 'New message', keys: S.keys.compose, run: () => openCompose({}) },
    { label: 'Check for new mail', keys: S.keys.refresh, run: refresh },
    { label: 'Search…', keys: S.keys.search, run: () => $('#q').focus() },
    { label: 'Go to All Inboxes', keys: S.keys['go-inbox'], run: () => navigate({ kind: 'query', q: 'in:inbox', title: 'All Inboxes', key: 'smart:inbox' }) },
    { label: 'Go to Unread', keys: S.keys['go-unread'], run: () => navigate({ kind: 'query', q: 'is:unread in:inbox', title: 'Unread', key: 'smart:unread' }) },
    { label: 'Go to Flagged', keys: S.keys['go-flagged'], run: () => navigate({ kind: 'query', q: 'is:flagged', title: 'Flagged', key: 'smart:flagged' }) },
    { label: 'Go to Sent', keys: S.keys['go-sent'], run: () => navigate({ kind: 'query', q: 'in:sent', title: 'Sent', key: 'smart:sent' }) },
    { label: 'Go to Drafts', keys: S.keys['go-drafts'], run: () => showPage('drafts') },
    { label: 'Settings', keys: S.keys['go-settings'], run: () => showPage('settings') },
    { label: 'Accounts', run: () => showPage('accounts') },
    { label: 'Add account', run: addAccountDialog },
    { label: 'Import from Apple Mail', run: importAppleDialog },
    { label: 'Rules', run: () => showPage('rules') },
    { label: 'Agents and activity', run: () => showPage('agents') },
    { label: 'Keyboard shortcuts', keys: S.keys.help, run: helpDialog },
  ];
  for (const t of [['system', 'Match system'], ['light', 'Light'], ['dark', 'Dark'], ['high-contrast', 'High contrast']]) c.push({ label: 'Theme: ' + t[1], run: () => putSetting('ui.theme', t[0]).catch(fail) });
  if (S.thread.length) c.push(
    { label: 'Reply', keys: S.keys.reply, run: () => respond('reply') }, { label: 'Reply all', keys: S.keys.replyall, run: () => respond('all') }, { label: 'Forward', keys: S.keys.forward, run: () => respond('forward') },
    { label: 'Archive', keys: S.keys.archive, run: () => act('archive') }, { label: 'Move to Trash', keys: S.keys.trash, run: () => act('trash') }, { label: 'Mark as junk', keys: S.keys.junk, run: () => act('junk') },
    { label: 'Flag or unflag', keys: S.keys.flag, run: toggleFlag }, { label: 'Mark read or unread', keys: S.keys.toggleread, run: toggleRead },
    { label: 'Snooze until tomorrow', run: () => act('snooze', 'tomorrow') });
  return c;
}
function openPalette() {
  const dlg = $('#palette'), input = $('#p-input'), list = $('#p-list');
  let items = [], idx = 0;
  const draw = () => {
    const q = input.value.trim().toLowerCase();
    items = commands().filter((c) => c.label.toLowerCase().includes(q));
    if (q) items.push({ label: `Search mail for “${input.value.trim()}”`, run: () => { $('#q').value = input.value.trim(); doSearch(); } });
    idx = Math.min(idx, Math.max(0, items.length - 1));
    list.replaceChildren(...items.map((c, i) => h('li', { role: 'option', id: 'pi-' + i, 'aria-selected': String(i === idx), onclick: () => run(i) }, h('span', { text: c.label }), c.keys ? h('kbd', { text: prettyKey(c.keys) }) : null)));
    input.setAttribute('aria-activedescendant', items.length ? 'pi-' + idx : '');
  };
  const run = (i) => { const c = items[i]; dlg.close(); if (c) setTimeout(c.run, 0); };
  input.value = ''; idx = 0; draw();
  input.oninput = () => { idx = 0; draw(); };
  input.onkeydown = (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); idx = (idx + 1) % items.length; draw(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); idx = (idx - 1 + items.length) % items.length; draw(); }
    else if (e.key === 'Enter') { e.preventDefault(); run(idx); }
  };
  dlg.showModal(); input.focus();
}
function helpDialog() {
  openDialog('Keyboard shortcuts', (body, foot, close) => {
    const t = h('table', { class: 'data' }, h('tbody', {}, Object.entries(KEY_LABEL).map(([k, l]) => h('tr', {}, h('td', { text: l }), h('td', {}, h('kbd', { text: prettyKey(S.keys[k]) }))))));
    body.append(t, h('p', { style: { color: 'var(--fg-3)' }, text: 'Change them in Settings.' }),
      h('h3', { text: 'Search syntax' }),
      h('table', { class: 'data' }, h('tbody', {}, [
        ['from:alice  to:bob  cc:carol', 'People'], ['subject:"q3 plan"', 'Subject words'], ['is:unread  is:flagged  is:vip  is:snoozed', 'State'], ['has:attachment', 'Attachments'],
        ['after:7d  before:2026-01-31  after:yesterday', 'Dates (d, w, m, y)'], ['in:inbox  in:sent  in:"Projects/Atlas"  account:me@x.com', 'Where'], ['budget "exact phrase"', 'Words in the message'],
      ].map(([a, b]) => h('tr', {}, h('td', { class: 'mono' }, a), h('td', { text: b }))))));
    foot.append(h('button', { class: 'btn primary', text: 'Close', onclick: close }));
  });
}

// ---------- keyboard ----------
function matchKey(e, spec) {
  if (!spec) return false;
  const parts = spec.split('+'), key = parts.pop();
  const mod = parts.includes('mod'), alt = parts.includes('alt'), shift = parts.includes('shift');
  if (mod !== (e.metaKey || e.ctrlKey)) return false;
  if (alt !== e.altKey) return false;
  if (shift && !e.shiftKey) return false;
  return e.key === key || e.key.toLowerCase() === key.toLowerCase() && (key.length > 1 || (!shiftNeeded(key) || e.shiftKey));
}
const shiftNeeded = (k) => k !== k.toLowerCase();
function onKey(e) {
  const t = e.target, typing = t.matches && t.matches('input, textarea, select, [contenteditable="true"]');
  const modalOpen = $$('dialog').some((d) => d.open);
  if (e.key === 'Escape' && !modalOpen) { if (document.activeElement === $('#q')) { $('#q').value = ''; $('#list').focus(); doSearch(); } return; }
  if (matchKey(e, S.keys.palette)) { e.preventDefault(); if (!$('#palette').open) openPalette(); return; }
  if (typing || modalOpen || e.metaKey || e.ctrlKey || e.altKey) return;
  const seq = S.pendingKey ? S.pendingKey + ' ' + e.key : e.key;
  const seqMatch = Object.entries(S.keys).find(([, v]) => v === seq);
  const prefix = Object.values(S.keys).some((v) => v.startsWith(e.key + ' ') && !S.pendingKey);
  clearTimeout(S.pendingTimer);
  if (S.pendingKey) { S.pendingKey = ''; }
  else if (prefix) { S.pendingKey = e.key; S.pendingTimer = setTimeout(() => { S.pendingKey = ''; }, 1200); e.preventDefault(); return; }
  const hit = seqMatch ? seqMatch[0] : Object.entries(S.keys).find(([, v]) => v === e.key)?.[0];
  if (!hit) { if (e.key === 'Delete' || e.key === 'Backspace') { if (S.thread.length) { e.preventDefault(); act('trash'); } } return; }
  e.preventDefault();
  const actions = {
    compose: () => openCompose({}), search: () => $('#q').focus(), next: () => step(1), prev: () => step(-1), open: () => S.sel && selectThread(S.sel, true),
    reply: () => respond('reply'), replyall: () => respond('all'), forward: () => respond('forward'), archive: () => act('archive'), trash: () => act('trash'), junk: () => act('junk'),
    flag: toggleFlag, toggleread: toggleRead, snooze: () => snoozeMenu($('#actions button[aria-label="Snooze (z)"]') || $('#list')), refresh,
    palette: openPalette, help: helpDialog,
    'go-inbox': () => navigate({ kind: 'query', q: 'in:inbox', title: 'All Inboxes', key: 'smart:inbox' }),
    'go-unread': () => navigate({ kind: 'query', q: 'is:unread in:inbox', title: 'Unread', key: 'smart:unread' }),
    'go-flagged': () => navigate({ kind: 'query', q: 'is:flagged', title: 'Flagged', key: 'smart:flagged' }),
    'go-sent': () => navigate({ kind: 'query', q: 'in:sent', title: 'Sent', key: 'smart:sent' }),
    'go-drafts': () => showPage('drafts'), 'go-settings': () => showPage('settings'),
  };
  (actions[hit] || (() => {}))();
}
function step(d) {
  if (!S.threads.length) return;
  let i = S.threads.findIndex((t) => t.thread_id === S.sel);
  i = Math.min(S.threads.length - 1, Math.max(0, i + d));
  selectThread(S.threads[i].thread_id, false);
}

// ---------- search & refresh ----------
function doSearch() {
  const q = $('#q').value.trim();
  if (!q) { navigate({ kind: 'query', q: 'in:inbox', title: 'All Inboxes', key: 'smart:inbox' }); return; }
  S.view = { kind: 'search', q, title: `Search: ${q}`, key: 'search' }; S.sel = null; S.thread = []; S.offset = 0; S.page = null;
  $('#app').removeAttribute('data-page'); renderSidebar(); renderReader(); loadList();
}
async function refresh() { try { await api('/api/sync', { method: 'POST', body: {} }); announce('Checking for new mail'); } catch (e) { fail(e); } }

async function refreshOverview(quiet) {
  try { S.ov = await api('/api/overview'); if (!quiet) renderSidebar(); } catch (e) { if (!quiet) fail(e); }
}

// ---------- live events ----------
function connectEvents() {
  const es = new EventSource('/api/events');
  const onSync = debounce(() => refreshOverview(), 500);
  const onMsg = debounce(async () => { const before = inboxUnread(); await refreshOverview(); if (S.view.kind !== 'page') loadList(); notifyNew(before); }, 700);
  es.onmessage = (ev) => {
    let e; try { e = JSON.parse(ev.data); } catch (_) { return; }
    if (e.type === 'message') onMsg();
    else if (e.type === 'sync') onSync();
    else if (e.type === 'account') { refreshOverview(); if (S.page === 'accounts') renderPage(); }
    else if (e.type === 'settings') api('/api/settings').then((s) => { S.settings = s; applySettings(); });
    else if (e.type === 'outbox') {
      loadDrafts(); refreshOverview();
      if (e.detail === 'sent') { announce('Message sent'); toast('Message sent'); }
      else if (e.detail === 'failed') toast('A message could not be sent. Open Drafts to retry.', { error: true });
      else if (e.detail === 'approval') toast('An agent drafted a message and is waiting for your approval.', { action: { label: 'Review', run: () => showPage('drafts') } });
    } else if (e.type === 'error') toast((e.account ? e.account + ': ' : '') + e.detail, { error: true });
  };
  es.onerror = () => { /* the browser reconnects on its own */ };
}

function notifyNew(before) {
  const now = inboxUnread();
  if (now > before && document.hidden && S.settings['ui.notifications'] === 'true' && 'Notification' in window && Notification.permission === 'granted') {
    const n = new Notification('OpenMail', { body: `${now - before} new message${now - before > 1 ? 's' : ''}`, tag: 'openmail-new', silent: false });
    n.onclick = () => { window.focus(); navigate({ kind: 'query', q: 'is:unread in:inbox', title: 'Unread', key: 'smart:unread' }); n.close(); };
  }
}

// ---------- boot ----------
async function boot() {
  try {
    [S.settings, S.ov] = await Promise.all([api('/api/settings'), api('/api/overview')]);
  } catch (e) { fail(e); return; }
  applySettings(); wireCompose();
  if (!/Mac|iPhone|iPad/.test(navigator.platform)) $('#send-kbd').textContent = 'Ctrl↵';
  $('#btn-compose-side').addEventListener('click', () => openCompose({}));
  $('#btn-refresh').addEventListener('click', refresh);
  $('#scrim').addEventListener('click', closeMenu);
  $('#menubtn').addEventListener('click', () => { const a = $('#app'); a.getAttribute('data-menu') === 'open' ? a.removeAttribute('data-menu') : a.setAttribute('data-menu', 'open'); });
  $('#q').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); doSearch(); $('#list').focus(); } });
  $$('.side-foot .side-item').forEach((b) => b.addEventListener('click', () => showPage(b.dataset.go)));
  $('#list').addEventListener('keydown', (e) => { if (e.key === 'ArrowDown') { e.preventDefault(); step(1); } else if (e.key === 'ArrowUp') { e.preventDefault(); step(-1); } else if (e.key === 'Enter') { e.preventDefault(); if (S.sel) selectThread(S.sel, true); } });
  document.addEventListener('keydown', onKey);
  $('#dlg').addEventListener('close', () => { $('#dlg-inner').replaceChildren(); });
  renderSidebar(); renderReader();
  await loadDrafts();
  if (!S.ov.accounts.length) { loadList(); addAccountDialog(); } else loadList();
  connectEvents();
  if (matchMedia('(prefers-color-scheme: dark)').addEventListener) matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { applySettings(); if (S.thread.length) renderReader(); });
}
// Entry points for the desktop shell (menus, mailto: links, notifications).
window.addEventListener('unhandledrejection', (e) => { try { toast((e.reason && e.reason.message) || 'Something went wrong', { error: true }); } catch (_) { /* ignore */ } });
window.OM = {
  newMessage: () => openCompose({}),
  refresh,
  palette: () => { if (!$('#palette').open) openPalette(); },
  mailto: (m) => openCompose({ to: parseRecipients(m.to || ''), cc: parseRecipients(m.cc || ''), subject: m.subject || '', body: m.body || '' }),
  go: (name) => {
    const smart = { inbox: SMART[0], unread: SMART[1], flagged: SMART[2] }[name];
    if (smart) navigate({ kind: 'query', q: smart.q, title: smart.title, key: smart.key });
    else showPage(name);
  },
};
boot().catch((e) => { console.error(e); try { connectEvents(); toast('Started with an error: ' + e.message, { error: true }); } catch (_) { /* ignore */ } });
