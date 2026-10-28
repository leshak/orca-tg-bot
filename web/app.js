'use strict';

// Веб-интерфейс orca-tg-bot. Данные приходят по SSE (/api/events):
//   state  — проекты и воркспейсы всех рантаймов;
//   terms  — терминалы открытого воркспейса;
//   screen — экран выбранного терминала.
// Команды уходят POST-запросами в /api/*.

const $ = (id) => document.getElementById(id);

// h('div.cls', {attr}, ...children) — элемент без innerHTML, текст всегда экранирован.
function h(tag, attrs, ...kids) {
  const [name, ...cls] = tag.split('.');
  const el = document.createElement(name);
  if (cls.length) el.className = cls.join(' ');
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid === undefined || kid === null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}

const store = {
  get(k, def) {
    try { const v = localStorage.getItem(k); return v === null ? def : JSON.parse(v); } catch { return def; }
  },
  set(k, v) {
    try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* приватный режим */ }
  },
};

const S = {
  snap: null,           // последний state
  sel: null,            // {env, id, term} — открытый воркспейс
  terms: null,          // последний terms
  screen: null,         // последний screen
  collapsed: new Set(store.get('collapsed', [])),
  compact: store.get('compact', matchMedia('(max-width: 799px)').matches),
  es: null,
};

// ---------- время ----------

function age(ms) {
  if (!ms) return '';
  const s = (Date.now() - ms) / 1000;
  if (s < 60) return 'сейчас';
  if (s < 3600) return Math.floor(s / 60) + 'м';
  if (s < 86400) return Math.floor(s / 3600) + 'ч';
  return Math.floor(s / 86400) + 'д';
}

// ---------- состояния ----------

const WS_STATE = {
  permission: 'ждёт разрешения',
  working: 'работает',
  ready: 'ждёт ответа',
  open: 'открыт',
  idle: 'спит',
};

const AGENT_STATE = {
  working: ['working', 'работает'],
  blocked: ['permission', 'ждёт разрешения'],
  waiting: ['ready', 'ждёт ввода'],
  done: ['ready', 'закончил'],
};

const dot = (st, label) => h('span.st.st-' + st, { role: 'img', 'aria-label': label || WS_STATE[st] || st, title: label || WS_STATE[st] || st });

const agentDot = (a) => {
  const [st, label] = AGENT_STATE[a.state] || ['idle', a.state];
  return dot(st, label);
};

const sameWS = (w, sel) => sel && w.env === sel.env && w.id === sel.id;

function findWS(sel) {
  if (!S.snap || !sel) return null;
  for (const p of S.snap.projects) {
    for (const w of p.workspaces) if (sameWS(w, sel)) return { p, w };
  }
  return null;
}

// ---------- список проектов ----------

const folderSVG = () => {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', 'M3 6.5A1.5 1.5 0 0 1 4.5 5h4.6l2 2h8.4A1.5 1.5 0 0 1 21 8.5v9a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5z');
  path.setAttribute('fill', 'none');
  path.setAttribute('stroke', 'currentColor');
  path.setAttribute('stroke-width', '1.6');
  svg.append(path);
  return svg;
};

function renderProjects() {
  const snap = S.snap;
  const nav = $('projects');
  const scroll = nav.scrollTop;
  nav.replaceChildren(...snap.projects.map(renderProject));
  nav.scrollTop = scroll;

  $('down').replaceChildren(...(snap.down || []).map((d) =>
    h('p', { title: d.error }, '🔌 ', d.name, ' не отвечает')));

  // Сводка: что требует внимания.
  const counts = {};
  for (const p of snap.projects) for (const w of p.workspaces) counts[w.state] = (counts[w.state] || 0) + 1;
  const parts = [];
  if (counts.permission) parts.push(h('span', { title: WS_STATE.permission }, '⚠️ ', counts.permission));
  if (counts.working) parts.push(h('span', { title: WS_STATE.working }, '⏳ ', counts.working));
  if (counts.ready) parts.push(h('span', { title: WS_STATE.ready }, '✅ ', counts.ready));
  $('summary').replaceChildren(...parts);
  const attention = (counts.permission || 0) + (counts.ready || 0);
  document.title = (counts.permission ? '⚠️ ' : '') + (attention ? `(${attention}) ` : '') + 'Orca';
}

function renderProject(p) {
  const collapsed = S.collapsed.has(p.id);
  const icon = p.icon ? h('img', { src: p.icon, alt: '', loading: 'lazy', referrerpolicy: 'no-referrer' }) : folderSVG();
  const head = h('div.proj-head', {},
    h('button.proj-toggle', {
      type: 'button',
      'aria-expanded': String(!collapsed),
      onclick: () => {
        if (S.collapsed.has(p.id)) S.collapsed.delete(p.id); else S.collapsed.add(p.id);
        store.set('collapsed', [...S.collapsed]);
        renderProjects();
      },
    }, icon, h('span.name', {}, p.name),
      collapsed && p.workspaces.length ? h('span.count', {}, p.workspaces.length) : null),
    p.repos.length ? h('button.icon-btn', {
      type: 'button', title: 'Новый воркtree', 'aria-label': 'Новый воркtree в ' + p.name,
      onclick: () => openWorktreeDialog(p),
    }, '+') : null,
  );
  const list = collapsed ? null : h('ul.ws-list', {}, p.workspaces.map((w) => h('li', {}, renderWS(w))));
  return h('section.proj', {}, head, list);
}

function renderWS(w) {
  const st = w.lost ? 'lost' : w.state;
  const sub = [];
  if (w.host) sub.push(h('span.badge', { title: w.host }, w.host));
  if (w.branch) sub.push(h('span.branch', {}, w.branch));
  if (w.lost) sub.push(h('span', {}, '🔌 нет связи с хостом'));
  if (w.comment) sub.push(h('span.comment', {}, '💬 ', w.comment));
  const agents = (w.agents || []).map((a) => h('div.ag', {},
    agentDot(a),
    h('span.ag-t', { title: a.title }, a.title || a.type || 'агент'),
    h('span.age', {}, a.state === 'working' && a.tool ? a.tool : age(a.updated))));
  return h('button.wsi', {
    type: 'button',
    'aria-current': sameWS(w, S.sel) ? 'true' : null,
    onclick: () => select({ env: w.env, id: w.id, term: '' }, true),
  },
  h('div.wsi-row', {}, dot(st, w.lost ? 'нет связи с хостом' : null), h('span.wsi-name', {}, w.name),
    w.main ? h('span.badge.outline', {}, 'primary') : null),
  sub.length ? h('div.wsi-sub', {}, sub) : null,
  agents);
}

// ---------- открытый воркспейс ----------

function activeAgent(w) {
  if (!w.agents || !w.agents.length) return null;
  const t = S.terms && S.terms.terms.find((x) => x.handle === S.terms.active);
  return (t && w.agents.find((a) => a.pane === t.pane)) || w.agents[0];
}

function renderHeader() {
  const found = findWS(S.sel);
  $('empty').hidden = !!S.sel;
  $('ws').hidden = !S.sel;
  if (!S.sel) return;
  if (!found) {
    $('ws-name').replaceChildren(S.snap ? 'Воркспейс не найден' : 'Загрузка…');
    $('ws-meta').replaceChildren();
    $('ws-msg').hidden = true;
    return;
  }
  const { p, w } = found;
  $('ws-name').replaceChildren(dot(w.lost ? 'lost' : w.state), w.name);
  const meta = [h('span', {}, p.name)];
  if (w.host) meta.push(h('span.badge', {}, w.host));
  if (w.branch) meta.push(h('span.branch', {}, w.branch));
  meta.push(h('span', {}, WS_STATE[w.state] || w.state));
  $('ws-meta').replaceChildren(...meta);

  const a = activeAgent(w);
  const msg = $('ws-msg');
  if (a && (a.message || a.tool)) {
    msg.hidden = false;
    const [, label] = AGENT_STATE[a.state] || ['', a.state];
    const head = [label];
    if (a.tool && (a.state === 'working' || a.state === 'blocked')) head.push('🔧 ' + a.tool);
    if (a.updated) head.push(age(a.updated));
    if (a.message) head.push('— ' + a.message.split('\n')[0]);
    $('ws-msg-head').replaceChildren(head.join(' · '));
    $('ws-msg-body').replaceChildren(a.message || '');
  } else {
    msg.hidden = true;
  }
}

function renderTabs() {
  const t = S.terms;
  const tabs = $('tabs');
  if (!t) { tabs.replaceChildren(); return; }
  if (t.error) { tabs.replaceChildren(h('span.tab', {}, '⚠️ ', t.error)); return; }
  if (!t.terms.length) { tabs.replaceChildren(h('span.tab', {}, 'Нет терминалов — запустите агента')); return; }
  const found = findWS(S.sel);
  tabs.replaceChildren(...t.terms.map((x) => {
    const a = found && found.w.agents && found.w.agents.find((ag) => ag.pane === x.pane);
    return h('button.tab', {
      type: 'button', role: 'tab',
      'aria-selected': String(x.handle === t.active),
      title: x.label + (x.writable ? '' : ' (только чтение)'),
      onclick: () => { if (x.handle !== t.active) select({ ...S.sel, term: x.handle }, false); },
    }, a ? agentDot(a) : null, x.lost ? '🔌 ' : '', x.label);
  }));
  const cur = tabs.querySelector('[aria-selected="true"]');
  if (cur) tabs.scrollLeft = cur.offsetLeft - tabs.offsetLeft - 8;
}

function renderScreen() {
  const pre = $('screen');
  const wrap = $('screen-wrap');
  const s = S.screen;
  const atBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 40;
  pre.classList.toggle('wrap', S.compact);
  if (!s) { pre.textContent = ''; return; }
  pre.classList.toggle('err', !!s.error);
  pre.textContent = s.error ? '⚠️ ' + s.error : (S.compact ? s.tidy : s.raw) || '(пусто)';
  if (atBottom) wrap.scrollTop = wrap.scrollHeight;
}

// ---------- выбор и адрес ----------

function hashOf(sel) {
  if (!sel) return '#';
  const q = new URLSearchParams({ env: sel.env, id: sel.id });
  if (sel.term) q.set('term', sel.term);
  return '#' + q.toString();
}

function selFromHash() {
  const q = new URLSearchParams(location.hash.slice(1));
  if (!q.get('id')) return null;
  return { env: q.get('env') || '', id: q.get('id'), term: q.get('term') || '' };
}

function select(sel, push) {
  const changedWS = !sameWS(sel || {}, S.sel);
  S.sel = sel;
  if (changedWS) { S.terms = null; S.screen = null; }
  const hash = hashOf(sel);
  if (location.hash !== hash) {
    if (push) history.pushState(null, '', hash); else history.replaceState(null, '', hash);
  }
  $('app').dataset.view = sel ? 'ws' : 'list';
  if (S.snap) renderProjects();
  renderHeader();
  renderTabs();
  renderScreen();
  connect();
  if (sel && changedWS && matchMedia('(min-width: 800px)').matches) $('msg').focus();
}

addEventListener('popstate', () => {
  const sel = selFromHash();
  if (JSON.stringify(sel) !== JSON.stringify(S.sel)) select(sel, false);
});

$('back').addEventListener('click', () => {
  if (history.state === null && history.length > 1 && selFromHash()) history.back();
  else select(null, true);
});

// ---------- поток событий ----------

function setConn(state) { $('conn').dataset.state = state; }

let retryTimer = null;

function connect() {
  if (S.es) S.es.close();
  clearTimeout(retryTimer);
  let url = '/api/events';
  if (S.sel) {
    const q = new URLSearchParams({ env: S.sel.env, wt: S.sel.id });
    if (S.sel.term) q.set('term', S.sel.term);
    url += '?' + q.toString();
  }
  const es = new EventSource(url);
  S.es = es;
  es.onopen = () => setConn('on');
  es.onerror = () => {
    setConn('off');
    if (es.readyState === EventSource.CLOSED) {
      // Браузер сдался (например, 401) — проверим вход и переподключимся сами.
      retryTimer = setTimeout(async () => {
        try {
          const r = await fetch('/api/ping');
          if (r.status === 401) { location.href = '/login'; return; }
        } catch { /* сеть недоступна — попробуем позже */ }
        if (S.es === es) connect();
      }, 3000);
    }
  };
  es.addEventListener('state', (e) => {
    S.snap = JSON.parse(e.data);
    renderProjects();
    renderHeader();
    renderTabs();
  });
  es.addEventListener('terms', (e) => {
    S.terms = JSON.parse(e.data);
    renderTabs();
    renderHeader();
  });
  es.addEventListener('screen', (e) => {
    const s = JSON.parse(e.data);
    if (S.terms && s.handle !== S.terms.active) return;
    S.screen = s;
    renderScreen();
  });
}

// Возраст «5м» в списке сам не обновится — перерисуем раз в полминуты.
setInterval(() => { if (S.snap) { renderProjects(); renderHeader(); } }, 30000);

// ---------- команды ----------

let toastTimer = null;
function toast(text, isError) {
  const t = $('toast');
  t.textContent = text;
  t.classList.toggle('error', !!isError);
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), isError ? 6000 : 2000);
}

async function post(path, body) {
  const r = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  let data = {};
  try { data = await r.json(); } catch { /* пустой ответ */ }
  if (r.status === 401) { location.href = '/login'; throw new Error('нужно войти'); }
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

const target = () => ({
  env: S.sel.env,
  wt: S.sel.id,
  term: (S.terms && S.terms.active) || S.sel.term || '',
});

const msgBox = $('msg');
const coarse = matchMedia('(pointer: coarse)');

$('composer').addEventListener('submit', async (e) => {
  e.preventDefault();
  const text = msgBox.value;
  if (!text.trim() || !S.sel) return;
  const btn = e.target.querySelector('.send');
  btn.disabled = true;
  try {
    await post('/api/send', { ...target(), text });
    msgBox.value = '';
    toast('Отправлено');
  } catch (err) {
    toast('Не отправлено: ' + err.message, true);
  } finally {
    btn.disabled = false;
  }
});

// Enter отправляет на компьютере; на телефоне Enter — перенос строки, отправка кнопкой.
msgBox.addEventListener('keydown', (e) => {
  if (e.key !== 'Enter' || e.shiftKey || coarse.matches) return;
  e.preventDefault();
  if (e.isComposing || e.keyCode === 229) return;
  $('composer').requestSubmit();
});

$('keys').addEventListener('click', async (e) => {
  const b = e.target.closest('button[data-key]');
  if (!b || !S.sel) return;
  try {
    await post('/api/key', { ...target(), key: b.dataset.key });
    if (b.dataset.key === 'ctrl-c') toast('Прерывание отправлено');
  } catch (err) {
    toast(err.message, true);
  }
});

$('compact').checked = S.compact;
$('compact').addEventListener('change', (e) => {
  S.compact = e.target.checked;
  store.set('compact', S.compact);
  renderScreen();
});

// ---------- диалоги ----------

function fillAgents(select) {
  const agents = (S.snap && S.snap.agents) || ['claude'];
  const def = (S.snap && S.snap.agent) || 'claude';
  select.replaceChildren(...agents.map((a) => h('option', { value: a, selected: a === def }, a)));
}

for (const d of document.querySelectorAll('dialog')) {
  d.addEventListener('click', (e) => { if (e.target.closest('[data-close]')) d.close(); });
}

$('add-agent').addEventListener('click', () => {
  const found = findWS(S.sel);
  if (!found) return;
  fillAgents($('agent-type'));
  $('dlg-agent-where').textContent = found.p.name + ' · ' + found.w.name;
  $('agent-err').textContent = '';
  $('dlg-agent').showModal();
});

$('form-agent').addEventListener('submit', async (e) => {
  e.preventDefault();
  const btn = e.submitter || e.target.querySelector('[type=submit]');
  btn.disabled = true;
  try {
    await post('/api/agent', { env: S.sel.env, wt: S.sel.id, agent: $('agent-type').value, prompt: $('agent-prompt').value });
    $('dlg-agent').close();
    $('agent-prompt').value = '';
    toast('Агент запущен');
  } catch (err) {
    $('agent-err').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

let wtProject = null;

function openWorktreeDialog(p) {
  wtProject = p;
  $('dlg-wt-title').textContent = 'Новый воркtree · ' + p.name;
  const repos = $('wt-repo');
  repos.replaceChildren(...p.repos.map((r, i) =>
    h('option', { value: String(i) }, r.env ? r.envName || r.env : 'Эта машина')));
  $('wt-repo-row').hidden = p.repos.length < 2;
  fillAgents($('wt-agent'));
  $('wt-err').textContent = '';
  $('dlg-wt').showModal();
}

$('form-wt').addEventListener('submit', async (e) => {
  e.preventDefault();
  const repo = wtProject.repos[Number($('wt-repo').value) || 0];
  const btn = e.submitter || e.target.querySelector('[type=submit]');
  btn.disabled = true;
  $('wt-err').textContent = '';
  try {
    const r = await post('/api/worktree', {
      env: repo.env, repo: repo.id, name: $('wt-name').value,
      agent: $('wt-agent').value, prompt: $('wt-prompt').value,
    });
    $('dlg-wt').close();
    $('wt-name').value = '';
    $('wt-prompt').value = '';
    toast('Воркtree создан');
    if (r.wt) select({ env: r.env, id: r.wt, term: '' }, true);
  } catch (err) {
    $('wt-err').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

// ---------- старт ----------

select(selFromHash(), false);
