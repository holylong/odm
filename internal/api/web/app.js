'use strict';

/* ══════════════════════════════════════════════════════════════════════════
   ODM desktop UI — vanilla JS, no build step, no dependencies.
   Talks to the loopback API in internal/api/server.go.
   ══════════════════════════════════════════════════════════════════════════ */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const state = {
  tasks: new Map(),
  order: [],
  settings: null,
  selected: null,
  filter: { kind: 'all', value: null },
  totalSpeed: 0,
  port: 0,
};

/* ───────────────────────────── formatting ───────────────────────────── */

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

function fmtBytes(n) {
  if (n === null || n === undefined || n < 0) return '—';
  let v = n, i = 0;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v.toFixed(0) : v.toFixed(v < 10 ? 2 : 1)) + ' ' + UNITS[i];
}

function fmtSpeed(bps) {
  if (!bps || bps < 1) return '0 B/s';
  return fmtBytes(bps) + '/s';
}

function fmtEta(remaining, bps) {
  if (!remaining || remaining <= 0) return '';
  if (!bps || bps < 1) return '—';
  let s = Math.round(remaining / bps);
  if (s < 0 || s > 86400 * 30) return '—';
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
}

const STATUS_LABEL = {
  queued: 'Queued',
  probing: 'Connecting',
  downloading: 'Downloading',
  paused: 'Paused',
  assembling: 'Assembling',
  complete: 'Complete',
  error: 'Error',
};

const EXT_ICON = {
  zip: '🗜', rar: '🗜', '7z': '🗜', gz: '🗜', tar: '📦', iso: '💿',
  mp4: '🎬', mkv: '🎬', avi: '🎬', mov: '🎬', webm: '🎬', flv: '🎬',
  mp3: '🎵', m4a: '🎵', wav: '🎵', flac: '🎵', ogg: '🎵',
  pdf: '📕', doc: '📄', docx: '📄', xls: '📊', xlsx: '📊', ppt: '📊', txt: '📄',
  exe: '⚙', msi: '⚙', apk: '📱', dmg: '💽', deb: '📦',
  png: '🖼', jpg: '🖼', jpeg: '🖼', gif: '🖼', svg: '🖼', webp: '🖼',
};

function iconFor(name) {
  if (!name) return '📄';
  const i = name.lastIndexOf('.');
  if (i < 0) return '📄';
  return EXT_ICON[name.slice(i + 1).toLowerCase()] || '📄';
}

/* ───────────────────────────── API ───────────────────────────── */

async function api(path, opts = {}) {
  const res = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

/* ───────────────────────────── derived task data ───────────────────────────── */

function downloaded(t) {
  if (!t.segments || !t.segments.length) return t.downloaded || 0;
  return t.segments.reduce((a, s) => a + Math.max(0, (s.pos || 0) - (s.start || 0)), 0);
}

function progress(t) {
  if (t.status === 'complete') return 1;
  if (!t.size || t.size <= 0) return 0;
  return Math.min(1, downloaded(t) / t.size);
}

function remaining(t) {
  if (!t.size || t.size <= 0) return -1;
  return Math.max(0, t.size - downloaded(t));
}

function matchesFilter(t) {
  const f = state.filter;
  switch (f.kind) {
    case 'all': return true;
    case 'unfinished': return t.status !== 'complete';
    case 'finished': return t.status === 'complete';
    case 'active': return ['downloading', 'probing', 'assembling', 'queued'].includes(t.status);
    case 'category': return (t.category || 'General') === f.value;
    default: return true;
  }
}

/* ───────────────────────────── rendering ───────────────────────────── */

function visibleTasks() {
  return state.order
    .map(id => state.tasks.get(id))
    .filter(Boolean)
    .filter(matchesFilter);
}

function renderSidebar() {
  const all = [...state.tasks.values()];
  const counts = new Map();
  for (const t of all) {
    const c = t.category || 'General';
    counts.set(c, (counts.get(c) || 0) + 1);
  }

  const groups = [
    { group: 'Downloads', items: [
      { kind: 'all', value: null, icon: '⤓', label: 'All downloads', count: all.length },
      { kind: 'active', value: null, icon: '⇣', label: 'In progress',
        count: all.filter(t => ['downloading', 'probing', 'assembling'].includes(t.status)).length },
      { kind: 'unfinished', value: null, icon: '◔', label: 'Unfinished',
        count: all.filter(t => t.status !== 'complete').length },
      { kind: 'finished', value: null, icon: '✓', label: 'Finished',
        count: all.filter(t => t.status === 'complete').length },
    ]},
    { group: 'Categories', items:
      [...counts.keys()].sort().map(c => ({
        kind: 'category', value: c, icon: catIcon(c), label: c, count: counts.get(c),
      })),
    },
  ];

  const wrap = $('#catList');
  wrap.textContent = '';
  for (const g of groups) {
    const h = document.createElement('div');
    h.className = 'catgroup';
    h.textContent = g.group;
    wrap.appendChild(h);
    for (const it of g.items) {
      const d = document.createElement('div');
      const active = state.filter.kind === it.kind && state.filter.value === it.value;
      d.className = 'cat' + (active ? ' active' : '');
      d.innerHTML = `<span class="cico">${it.icon}</span><span>${it.label}</span>` +
                    `<span class="ccount">${it.count}</span>`;
      d.onclick = () => {
        state.filter = { kind: it.kind, value: it.value };
        renderSidebar();
        renderRows();
      };
      wrap.appendChild(d);
    }
  }
}

function catIcon(c) {
  return ({ Video: '🎬', Music: '🎵', Documents: '📄', Programs: '⚙', Compressed: '🗜' })[c] || '📁';
}

function renderRows() {
  const list = visibleTasks();
  const rows = $('#rows');
  $('#empty').hidden = list.length > 0;
  rows.textContent = '';

  for (const t of list) {
    const p = progress(t);
    const rem = remaining(t);
    const el = document.createElement('div');
    el.className = 'row' + (state.selected === t.id ? ' sel' : '');
    el.dataset.id = t.id;

    const ticks = (t.segments || [])
      .slice()
      .sort((a, b) => a.start - b.start)
      .filter(s => s.start > 0)
      .map(s => {
        const pct = t.size > 0 ? (s.start / t.size) * 100 : 0;
        return `<i class="tick" style="left:${pct.toFixed(3)}%"></i>`;
      }).join('');

    const statusKey = t.status || 'queued';
    const speed = statusKey === 'downloading' ? fmtSpeed(t.speed) : '';
    const eta = statusKey === 'downloading' ? fmtEta(rem, t.speed) : '';

    el.innerHTML = `
      <div class="c-file">
        <span class="fileico">${iconFor(t.filename)}</span>
        <span class="fname" title="${escapeAttr(t.filename || '')}">${escapeHtml(t.filename || '(resolving…)')}</span>
      </div>
      <div class="status s-${statusKey}" title="${escapeAttr(t.error || '')}">
        <span class="sdot"></span><span>${STATUS_LABEL[statusKey] || statusKey}</span>
      </div>
      <div class="mono">${fmtBytes(t.size)}</div>
      <div class="progwrap">
        <div class="progbar" title="${(p * 100).toFixed(1)}%">
          <div class="progfill ${statusKey === 'complete' ? 'done' : ''}" style="width:${(p * 100).toFixed(2)}%"></div>
          ${ticks}
        </div>
        <span class="progtxt">${Math.floor(p * 100)}%</span>
      </div>
      <div class="mono">${speed}</div>
      <div class="mono">${eta}</div>
      <div class="c-desc" title="${escapeAttr(t.finalURL || t.url || '')}">${escapeHtml(t.finalURL || t.url || '')}</div>
    `;

    el.onclick = () => selectTask(t.id);
    el.ondblclick = () => {
      if (t.status === 'complete') {
        navigator.clipboard?.writeText(t.dir + '\\' + t.filename).catch(() => {});
        status(`Path copied: ${t.dir}\\${t.filename}`);
      } else if (['downloading', 'probing', 'queued'].includes(t.status)) {
        api(`/tasks/${t.id}/pause`, { method: 'POST' });
      } else {
        api(`/tasks/${t.id}/resume`, { method: 'POST' });
      }
    };
    rows.appendChild(el);
  }
  renderStats();
}

function renderStats() {
  const all = [...state.tasks.values()];
  const active = all.filter(t => ['downloading', 'probing', 'assembling'].includes(t.status));
  const totalBytes = all.reduce((a, t) => a + (t.size > 0 ? t.size : 0), 0);
  const doneBytes = all.reduce((a, t) => a + downloaded(t), 0);

  $('#totalSpeed').textContent = fmtSpeed(state.totalSpeed);
  $('#activeCount').textContent = active.length ? `${active.length} active` : 'idle';
  $('#statusStats').textContent =
    `${all.length} files · ${fmtBytes(doneBytes)} of ${fmtBytes(totalBytes)}`;
  $('#statusPort').textContent = state.port ? `127.0.0.1:${state.port}` : '—';
}

function selectTask(id) {
  state.selected = id;
  $$('.row').forEach(r => r.classList.toggle('sel', r.dataset.id === id));
  renderDetail();
}

function renderDetail() {
  const t = state.tasks.get(state.selected);
  const panel = $('#detail');
  if (!t) { panel.hidden = true; return; }
  panel.hidden = false;
  $('#dName').textContent = t.filename || '(resolving…)';

  // segment map: proportional band per segment with its own fill
  const bar = $('#segbar');
  bar.textContent = '';
  // The engine appends new halves, so order the band by offset before drawing.
  const segs = ((t.segments && t.segments.length) ? t.segments.slice() : [{ start: 0, end: (t.size || 1) - 1, pos: 0 }])
    .sort((a, b) => a.start - b.start);
  const total = Math.max(1, t.size > 0 ? t.size : segs.reduce((a, s) => a + (s.end - s.start + 1), 0));
  for (const s of segs) {
    const width = ((s.end - s.start + 1) / total) * 100;
    const done = Math.max(0, Math.min(1, ((s.pos || 0) - s.start) / Math.max(1, s.end - s.start + 1)));
    const d = document.createElement('div');
    d.className = 'seg' + (s.done ? ' done' : '');
    d.style.width = width.toFixed(4) + '%';
    d.title = `[${s.start}–${s.end}] ${(done * 100).toFixed(1)}%`;
    d.innerHTML = `<div class="fill" style="width:${(done * 100).toFixed(2)}%"></div>`;
    bar.appendChild(d);
  }

  const dl = $('#dInfo');
  const rows = [
    ['Status', STATUS_LABEL[t.status] || t.status],
    ['Segments', String(segs.length)],
    ['Connections', String(t.connections || 1)],
    ['Resume', t.resumeSupport ? 'yes' : 'no'],
    ['Size', fmtBytes(t.size)],
    ['Downloaded', `${fmtBytes(downloaded(t))} (${(progress(t) * 100).toFixed(1)}%)`],
    ['Speed', t.status === 'downloading' ? fmtSpeed(t.speed) : '—'],
    ['MIME', t.mime || '—'],
    ['Save to', t.dir || '—'],
    ['URL', t.finalURL || t.url || '—'],
    ['Last modified', t.lastModTime || '—'],
    ['Queue', t.queueID || '—'],
  ];
  if (t.error) rows.push(['Error', t.error]);
  dl.innerHTML = rows.map(([k, v]) =>
    `<div><dt>${k}</dt><dd title="${escapeAttr(String(v))}">${escapeHtml(String(v))}</dd></div>`).join('');
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}
function escapeAttr(s) { return escapeHtml(s); }

function status(msg) { $('#statusMsg').textContent = msg; }

/* ───────────────────────────── live updates ───────────────────────────── */

function upsertTask(t) {
  const existed = state.tasks.has(t.id);
  state.tasks.set(t.id, t);
  if (!existed) state.order.push(t.id);
}

function connectEvents() {
  const es = new EventSource('/api/events');

  es.addEventListener('open', () => {
    $('#connDot').classList.add('on');
    $('#connText').textContent = 'connected';
  });

  es.addEventListener('snapshot', e => {
    const data = JSON.parse(e.data);
    state.tasks.clear();
    state.order = [];
    for (const t of data.tasks) upsertTask(t);
    state.totalSpeed = data.totalSpeed || 0;
    renderSidebar(); renderRows(); renderDetail();
  });

  es.addEventListener('task', e => {
    const { type, taskID, task } = JSON.parse(e.data);
    if (type === 'removed') {
      state.tasks.delete(taskID);
      state.order = state.order.filter(id => id !== taskID);
      if (state.selected === taskID) { state.selected = null; }
    } else if (task) {
      upsertTask(task);
      if (type === 'added') status(`Added: ${task.filename || task.url}`);
      if (task.status === 'complete') status(`Complete: ${task.filename}`);
      if (task.status === 'error') status(`Error: ${task.error || task.filename}`);
    }
    renderSidebar(); renderRows(); renderDetail();
  });

  es.addEventListener('log', e => {
    const { line } = JSON.parse(e.data);
    appendLog(line);
  });

  es.onerror = () => {
    $('#connDot').classList.remove('on');
    $('#connText').textContent = 'reconnecting…';
  };
}

/* ───────────────────────────── log drawer ───────────────────────────── */

function appendLog(line) {
  const body = $('#logBody');
  if (!body) return;
  const atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 24;
  const div = document.createElement('div');
  if (/error|fail|cannot|denied/i.test(line)) div.className = 'err';
  else if (/^(probe|split|assembled|bridge)/.test(line)) div.className = 'dim';
  div.textContent = line;
  body.appendChild(div);
  while (body.childElementCount > 800) body.removeChild(body.firstChild);
  if (atBottom) body.scrollTop = body.scrollHeight;
}

/* ───────────────────────────── modals ───────────────────────────── */

function openModal(sel) { $(sel).hidden = false; }
function closeModal(sel) { $(sel).hidden = true; }

function wireModals() {
  $$('[data-close]').forEach(b => {
    b.onclick = () => b.closest('.modal-wrap').hidden = true;
  });
  $$('.modal-wrap').forEach(w => {
    w.addEventListener('mousedown', e => { if (e.target === w) w.hidden = true; });
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') $$('.modal-wrap').forEach(w => w.hidden = true);
  });
}

/* add URL */
function openAdd(prefill = {}) {
  if (state.settings) {
    $('#addDir').value = prefill.dir || state.settings.DefaultDir || state.settings.defaultDir || '';
  }
  $('#addUrl').value = prefill.url || '';
  $('#addName').value = prefill.filename || '';
  $('#addRef').value = prefill.referer || '';
  $('#addCat').value = prefill.category || 'General';
  $('#addQueue').value = 'main';
  $('#addSilent').checked = false;
  $('#addHint').textContent = '';
  openModal('#addModal');
  setTimeout(() => $('#addUrl').focus(), 30);
}

async function submitAdd() {
  const url = $('#addUrl').value.trim();
  if (!url) { $('#addHint').textContent = 'A URL is required.'; return; }
  try {
    const t = await api('/tasks', {
      method: 'POST',
      body: {
        url,
        dir: $('#addDir').value.trim(),
        filename: $('#addName').value.trim(),
        referer: $('#addRef').value.trim(),
        category: $('#addCat').value.trim() || 'General',
        queueID: $('#addQueue').value.trim(),
        startNow: !$('#addSilent').checked,
      },
    });
    upsertTask(t);
    closeModal('#addModal');
    selectTask(t.id);
    renderSidebar(); renderRows();
  } catch (err) {
    $('#addHint').textContent = 'Could not add: ' + err.message;
  }
}

/* options */
function fillOptions() {
  const s = state.settings || {};
  const p = s.proxy || {};
  const ft = s.fileTypes || s.filetypes || {};
  $('#optConns').value = s.maxConnectionsNumber ?? 8;
  $('#optConc').value = s.maxConcurrentDownloads ?? 3;
  $('#optMinSeg').value = s.minSegmentSize ?? 262144;
  $('#optSpeed').value = s.speedLimitKBps ?? 0;
  $('#optTimeout').value = s.timeoutSec ?? 30;
  $('#optRetries').value = s.maxRetries ?? 5;
  $('#optUA').value = s.userAgent || '';
  $('#optIgnoreMod').checked = !!s.ignoreModTimeChange;
  $('#optDir').value = s.defaultDir || '';
  $('#optTemp').value = s.tempDir || '';
  $('#optPort').value = s.bridgePort ?? 61161;
  $('#optClip').checked = !!s.monitorClipboard;
  $('#optAuto').value = (ft.autoStart || []).join(', ');
  $('#optExSites').value = (ft.excludedSites || []).join(', ');
  $('#optExAddr').value = (ft.excludedAddress || []).join(', ');
  $('#optProxyEnable').checked = !!p.enable;
  $('#optHttpProxy').value = p.httpProxy || '';
  $('#optHttpPort').value = p.httpPort ?? 8080;
  $('#optHttpUser').value = p.httpUser || '';
  $('#optHttpPass').value = p.httpPass || '';
  $('#optSocksHost').value = p.socksHost || '';
  $('#optSocksPort').value = p.socksPort ?? 1080;
  $('#optSocksType').value = p.socksType ?? 5;
  $('#optSocksEnable').checked = !!p.useSocks;
}

function splitList(v) {
  return v.split(/[,;\n]/).map(s => s.trim()).filter(Boolean);
}

async function saveOptions() {
  const s = JSON.parse(JSON.stringify(state.settings || {}));
  s.maxConnectionsNumber = +$('#optConns').value;
  s.maxConcurrentDownloads = +$('#optConc').value;
  s.minSegmentSize = +$('#optMinSeg').value;
  s.speedLimitKBps = +$('#optSpeed').value;
  s.timeoutSec = +$('#optTimeout').value;
  s.maxRetries = +$('#optRetries').value;
  s.userAgent = $('#optUA').value;
  s.ignoreModTimeChange = $('#optIgnoreMod').checked;
  s.defaultDir = $('#optDir').value;
  s.tempDir = $('#optTemp').value;
  s.bridgePort = +$('#optPort').value;
  s.monitorClipboard = $('#optClip').checked;
  s.fileTypes = s.fileTypes || {};
  s.fileTypes.autoStart = splitList($('#optAuto').value);
  s.fileTypes.excludedSites = splitList($('#optExSites').value);
  s.fileTypes.excludedAddress = splitList($('#optExAddr').value);
  s.proxy = s.proxy || {};
  s.proxy.enable = $('#optProxyEnable').checked;
  s.proxy.httpProxy = $('#optHttpProxy').value;
  s.proxy.httpPort = +$('#optHttpPort').value;
  s.proxy.httpUser = $('#optHttpUser').value;
  s.proxy.httpPass = $('#optHttpPass').value;
  s.proxy.socksHost = $('#optSocksHost').value;
  s.proxy.socksPort = +$('#optSocksPort').value;
  s.proxy.socksType = +$('#optSocksType').value;
  s.proxy.useSocks = $('#optSocksEnable').checked;

  const saved = await api('/settings', { method: 'POST', body: s });
  state.settings = saved;
  closeModal('#optModal');
  status('Options saved — connection settings apply to the next download start.');
  renderSidebarFoot();
}

function renderSidebarFoot() {
  const s = state.settings || {};
  $('#sfConns').textContent = s.maxConnectionsNumber ?? '—';
  $('#sfConc').textContent = s.maxConcurrentDownloads ?? '—';
  const dir = s.defaultDir || '—';
  $('#sfDir').textContent = dir;
  $('#sfDir').title = dir;
}

/* grabber */
let lastGrabResult = null;

async function runGrabber() {
  const body = {
    name: 'ui-project',
    startURL: $('#gStart').value.trim(),
    depth: +$('#gDepth').value,
    maxFiles: +$('#gMax').value,
    fileFilter: splitList($('#gFilter').value),
    includePath: splitList($('#gInc').value),
    excludePath: splitList($('#gExc').value),
    sameSite: $('#gSameSite').checked,
    minSize: parseSize($('#gMin').value),
    maxSize: parseSize($('#gMaxSize').value),
  };
  const box = $('#grabResults');
  box.innerHTML = '<div class="res">Scanning…</div>';
  $('#grabAdd').disabled = true;
  try {
    const res = await api('/grabber/run', { method: 'POST', body });
    lastGrabResult = res;
    box.textContent = '';
    if (!res.files.length) {
      box.innerHTML = `<div class="res">No matches. Pages scanned: ${res.pages}, skipped: ${res.skipped}</div>`;
    }
    for (const f of res.files) {
      const d = document.createElement('div');
      d.className = 'res';
      d.innerHTML = `<span class="rname" title="${escapeAttr(f.url)}">${escapeHtml(f.name)}</span>
                     <span class="rsize">${fmtBytes(f.size)}</span>`;
      box.appendChild(d);
    }
    for (const e of res.errors || []) {
      const d = document.createElement('div');
      d.className = 'res err';
      d.textContent = e;
      box.appendChild(d);
    }
    $('#grabAdd').disabled = res.files.length === 0;
    status(`Grabber: ${res.files.length} matches from ${res.pages} page(s)`);
  } catch (err) {
    box.innerHTML = `<div class="res err">${escapeHtml(err.message)}</div>`;
  }
}

function parseSize(v) {
  v = (v || '').trim().toLowerCase();
  if (!v) return 0;
  const m = v.match(/^([\d.]+)\s*(gb?|mb?|kb?|b?)$/);
  if (!m) return 0;
  const n = parseFloat(m[1]);
  const mult = { g: 1 << 30, m: 1 << 20, k: 1 << 10, b: 1 }[m[2][0]] || 1;
  return Math.round(n * mult);
}

async function addGrabberMatches() {
  if (!lastGrabResult) return;
  const dir = (state.settings && state.settings.defaultDir) || '';
  let n = 0;
  for (const f of lastGrabResult.files) {
    try {
      const t = await api('/tasks', {
        method: 'POST',
        body: {
          url: f.url,
          filename: f.name,
          dir,
          referer: lastGrabResult.project.startURL,
          category: 'General',
          queueID: 'grabber',
          startNow: false,
        },
      });
      upsertTask(t); n++;
    } catch { /* keep going */ }
  }
  closeModal('#grabModal');
  renderSidebar(); renderRows();
  status(`Queued ${n} grabber match(es).`);
}

/* ───────────────────────────── wiring ───────────────────────────── */

function wireToolbar() {
  $('#btnAdd').onclick = () => openAdd();
  $('#emptyAdd').onclick = () => openAdd();
  $('#addBrowse').onclick = () => status('Type the folder path: the browser cannot open a native folder picker here.');
  $('#addOk').onclick = submitAdd;
  $('#addUrl').onkeydown = e => { if (e.key === 'Enter') submitAdd(); };

  $('#btnSettings').onclick = () => { fillOptions(); openModal('#optModal'); };
  $('#optSave').onclick = () => saveOptions().catch(e => status('Save failed: ' + e.message));
  $$('#optTabs .tab').forEach(tab => {
    tab.onclick = () => {
      $$('#optTabs .tab').forEach(t => t.classList.toggle('active', t === tab));
      $$('.tabpane').forEach(p => p.classList.toggle('active', p.dataset.pane === tab.dataset.tab));
    };
  });

  $('#btnGrab').onclick = () => openModal('#grabModal');
  $('#grabRun').onclick = runGrabber;
  $('#grabAdd').onclick = addGrabberMatches;

  $('#btnStopAll').onclick = async () => {
    const active = [...state.tasks.values()].filter(t =>
      ['downloading', 'probing', 'queued', 'assembling'].includes(t.status));
    for (const t of active) await api(`/tasks/${t.id}/pause`, { method: 'POST' }).catch(() => {});
    status(`Stopped ${active.length} download(s).`);
  };

  $('#btnStartQueue').onclick = async () => {
    const idle = [...state.tasks.values()].filter(t => ['paused', 'error', 'queued'].includes(t.status));
    for (const t of idle) await api(`/tasks/${t.id}/resume`, { method: 'POST' }).catch(() => {});
    status(`Resumed ${idle.length} download(s).`);
  };

  $('#btnRemove').onclick = async () => {
    if (!state.selected) { status('Select a download first.'); return; }
    const t = state.tasks.get(state.selected);
    const alsoFile = t && t.status === 'complete'
      ? confirm('Also delete the downloaded file from disk?')
      : false;
    await api(`/tasks/${state.selected}/remove?file=${alsoFile ? 1 : 0}`, { method: 'POST' });
    if (alsoFile) status('Removed download and file.');
  };

  $('#dClose').onclick = () => { state.selected = null; renderRows(); renderDetail(); };

  $$('.menu button').forEach(b => {
    b.onclick = () => {
      const m = b.dataset.menu;
      if (m === 'options') { fillOptions(); openModal('#optModal'); }
      else if (m === 'grabber') openModal('#grabModal');
      else if (m === 'logs') $('#logDrawer').hidden = !$('#logDrawer').hidden;
      else { state.filter = { kind: 'all', value: null }; renderSidebar(); renderRows(); }
    };
  });
  $('#logClose').onclick = () => { $('#logDrawer').hidden = true; };

  document.addEventListener('keydown', e => {
    if (e.key === 'Delete' && state.selected) $('#btnRemove').click();
    if ((e.ctrlKey || e.metaKey) && e.key === 'n') { e.preventDefault(); openAdd(); }
  });
}

/* ───────────────────────────── boot ───────────────────────────── */

async function boot() {
  wireModals();
  wireToolbar();

  try {
    const snap = await api('/state');
    state.settings = snap.settings;
    state.port = snap.port;
    for (const t of snap.tasks) upsertTask(t);
    state.totalSpeed = snap.totalSpeed || 0;
  } catch (err) {
    status('Cannot reach the ODM service: ' + err.message);
  }

  renderSidebarFoot();
  renderSidebar();
  renderRows();
  connectEvents();

  // periodic full state refresh keeps totals honest even if an SSE frame is lost
  setInterval(async () => {
    try {
      const snap = await api('/state');
      state.totalSpeed = snap.totalSpeed || 0;
      for (const t of snap.tasks) {
        const prev = state.tasks.get(t.id);
        if (!prev || JSON.stringify(prev) !== JSON.stringify(t)) {
          state.tasks.set(t.id, t);
          if (!prev) state.order.push(t.id);
        }
      }
      state.settings = snap.settings;
      renderRows();
    } catch { /* offline is handled by the SSE indicator */ }
  }, 5000);
}

boot();
