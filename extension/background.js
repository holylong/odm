'use strict';

/* ══════════════════════════════════════════════════════════════════════════
   ODM Integration Module — background service worker.

   Two transports, in this order, exactly like the shipped IDM extension:
     1. native messaging host  "com.odm.host"  (preferred)
     2. loopback HTTP bridge    http://127.0.0.1:<port>/api/bridge/takeover

   The extension never decides on its own whether a URL is a download; ODM owns
   the File Types list, the excluded sites and the size filters. The extension
   only reports what the browser knows.
   ══════════════════════════════════════════════════════════════════════════ */

const HOST_NAME = 'com.odm.host';
const DEFAULT_PORT = 61161;
const PORT_PROBE_RANGE = 12;

/* ─────────────────────────── port discovery ─────────────────────────── */

let cachedPort = null;

async function discoverPort() {
  if (cachedPort) return cachedPort;
  const stored = await chrome.storage.local.get('port');
  if (stored.port) { cachedPort = stored.port; return cachedPort; }
  for (let p = DEFAULT_PORT; p < DEFAULT_PORT + PORT_PROBE_RANGE; p++) {
    try {
      const res = await fetch(`http://127.0.0.1:${p}/api/bridge/ping`, { cache: 'no-store' });
      if (res.ok) {
        const body = await res.json();
        if (body && body.ok) {
          cachedPort = p;
          await chrome.storage.local.set({ port: p });
          return p;
        }
      }
    } catch { /* next port */ }
  }
  return null;
}

/* ─────────────────────────── transports ─────────────────────────── */

function sendNative(payload) {
  return new Promise((resolve, reject) => {
    let port;
    try {
      port = chrome.runtime.connectNative(HOST_NAME);
    } catch (err) {
      reject(err);
      return;
    }
    const timer = setTimeout(() => {
      try { port.disconnect(); } catch {}
      reject(new Error('native host timeout'));
    }, 5000);

    port.onMessage.addListener(msg => {
      clearTimeout(timer);
      try { port.disconnect(); } catch {}
      resolve(msg);
    });
    port.onDisconnect.addListener(() => {
      clearTimeout(timer);
      const err = chrome.runtime.lastError;
      reject(new Error(err ? err.message : 'native host closed'));
    });
    port.postMessage(payload);
  });
}

async function sendHTTP(payload) {
  const port = await discoverPort();
  if (!port) throw new Error('ODM service not running');
  const res = await fetch(`http://127.0.0.1:${port}/api/bridge/takeover`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  if (!res.ok) throw new Error('bridge HTTP ' + res.status);
  return res.json();
}

async function send(payload) {
  try {
    return await sendNative(payload);
  } catch {
    return await sendHTTP(payload);
  }
}

/* ─────────────────────────── request building ─────────────────────────── */

function filenameFromURL(u) {
  try {
    const url = new URL(u);
    const last = url.pathname.split('/').filter(Boolean).pop() || '';
    return decodeURIComponent(last);
  } catch {
    return '';
  }
}

function extensionOf(url, filename, mime) {
  if (filename && filename.includes('.')) {
    return filename.split('.').pop().toLowerCase();
  }
  const f = filenameFromURL(url);
  if (f && f.includes('.')) return f.split('.').pop().toLowerCase();
  if (mime) {
    const guess = {
      'application/zip': 'zip',
      'application/pdf': 'pdf',
      'application/x-msdownload': 'exe',
      'video/mp4': 'mp4',
      'audio/mpeg': 'mp3',
      'application/x-7z-compressed': '7z',
      'application/vnd.rar': 'rar',
    }[mime.split(';')[0].trim()];
    if (guess) return guess;
  }
  return '';
}

async function cookiesFor(url) {
  try {
    const list = await chrome.cookies.getAll({ url });
    const out = {};
    for (const c of list) out[c.name] = c.value;
    return out;
  } catch {
    return {};
  }
}

async function buildPayload(url, referer, tabId) {
  const filename = filenameFromURL(url);
  const cookies = await cookiesFor(url);
  return {
    type: 'takeover',
    url,
    referer: referer || '',
    filename,
    mime: '',
    size: 0,
    userAgent: navigator.userAgent,
    tabID: tabId || 0,
    cookies,
    silent: false,
  };
}

/* ─────────────────────────── download interception ─────────────────────────── */

// A navigation that ends in a non-HTML response is a download the browser is
// about to take over. Cancel it and hand it to ODM instead.
async function interceptNow(details) {
  const { url, tabId, referer } = details;
  if (!url.startsWith('http')) return;

  const payload = await buildPayload(url, referer, tabId);
  let reply;
  try {
    reply = await send(payload);
  } catch (err) {
    console.warn('[ODM] bridge unavailable:', err.message);
    return;
  }
  if (!reply || !reply.takeover) {
    console.debug('[ODM] not taking over:', reply && reply.reason);
    return;
  }
  await chrome.downloads.cancel(details.id).catch(() => {});
  await chrome.downloads.erase({ id: details.id }).catch(() => {});
  chrome.notifications?.create?.({
    type: 'basic',
    iconUrl: 'icons/logo48.png',
    title: 'ODM',
    message: `Downloading with ODM: ${payload.filename || url}`,
  });
}

chrome.downloads.onCreated.addListener(async item => {
  const state = await chrome.storage.local.get('enabled');
  if (state.enabled === false) return;
  await interceptNow({ id: item.id, url: item.finalUrl || item.url, referer: item.referrer, tabId: 0 });
});

chrome.webRequest.onHeadersReceived.addListener(
  details => {
    if (details.tabId < 0) return;
    const type = details.type;
    if (type !== 'main_frame' && type !== 'sub_frame') return;
    const disp = (details.responseHeaders || [])
      .find(h => h.name.toLowerCase() === 'content-disposition');
    if (!disp) return;
    if (!/attachment/i.test(disp.value || '')) return;
    const nameMatch = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(disp.value || '');
    const filename = nameMatch ? decodeURIComponent(nameMatch[1]) : '';
    void send({
      type: 'takeover',
      url: details.url,
      referer: details.initiator || '',
      filename,
      mime: '',
      size: 0,
      userAgent: navigator.userAgent,
      tabID: details.tabId,
      silent: true,
    }).catch(() => {});
  },
  { urls: ['<all_urls>'], types: ['main_frame', 'sub_frame'] },
  ['responseHeaders']
);

/* ─────────────────────────── messages from the popup/content ─────────────────────────── */

chrome.runtime.onMessage.addListener((msg, sender, reply) => {
  if (msg.type === 'takeover-request') {
    (async () => {
      const payload = await buildPayload(msg.url, msg.referer, sender.tab?.id);
      try {
        const res = await send(payload);
        reply({ ok: true, result: res });
      } catch (err) {
        reply({ ok: false, error: err.message });
      }
    })();
    return true; // async reply
  }

  if (msg.type === 'status') {
    (async () => {
      try {
        const res = await sendNative({ type: 'ping' });
        reply({ ok: true, native: res, port: cachedPort });
      } catch {
        const port = await discoverPort();
        reply({ ok: !!port, native: null, port });
      }
    })();
    return true;
  }

  if (msg.type === 'set-enabled') {
    chrome.storage.local.set({ enabled: msg.enabled });
    reply({ ok: true });
  }
  return false;
});

chrome.runtime.onInstalled.addListener(async () => {
  const cur = await chrome.storage.local.get(['enabled']);
  if (cur.enabled === undefined) await chrome.storage.local.set({ enabled: true });
});
