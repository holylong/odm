'use strict';

/* ODM popup: status, takeover switch, manual URL, media list. */

const $ = s => document.querySelector(s);

function setStatus(on, text) {
  $('#dot').classList.toggle('on', !!on);
  $('#statusText').textContent = text;
}

async function checkService() {
  setStatus(false, 'checking…');
  const reply = await chrome.runtime.sendMessage({ type: 'status' }).catch(() => null);
  if (reply && reply.native) {
    setStatus(true, `native ${reply.native.version || ''}`.trim());
    $('#note').innerHTML =
      'Connected through the native messaging host <code>com.odm.host</code>.';
    return;
  }
  if (reply && reply.port) {
    setStatus(true, `bridge :${reply.port}`);
    $('#note').innerHTML =
      'Connected through the loopback bridge on port <code>' + reply.port + '</code>.';
    return;
  }
  setStatus(false, 'ODM not running');
  $('#note').innerHTML =
    'Start the ODM service (<code>odm --serve</code>) and reopen this popup.';
}

async function loadEnabled() {
  const { enabled } = await chrome.storage.local.get('enabled');
  $('#enabled').checked = enabled !== false;
}

async function scanMedia() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab || !tab.id) return;
  const box = $('#media');
  box.textContent = '';
  let scan = null;
  try {
    scan = await chrome.tabs.sendMessage(tab.id, { type: 'scan-media' });
  } catch {
    box.innerHTML = '<div class="empty">Cannot scan this page (no content script).</div>';
    return;
  }
  const media = (scan && scan.media) || [];
  if (!media.length) {
    box.innerHTML = '<div class="empty">No media or file links found on this page.</div>';
    return;
  }
  for (const m of media) {
    const row = document.createElement('div');
    row.className = 'm';
    const icon = { video: '🎬', audio: '🎵', link: '📄', source: '📦', data: '🔗' }[m.kind] || '🔗';
    row.innerHTML = `<span class="kind">${icon}</span>
                     <span class="url" title="${m.url}">${m.url.split('/').pop() || m.url}</span>
                     <button>Get</button>`;
    row.querySelector('button').onclick = async () => {
      const reply = await chrome.runtime.sendMessage({ type: 'takeover-request', url: m.url, referer: scan.page.url });
      if (reply && reply.ok) row.querySelector('button').textContent = 'Sent';
      else row.querySelector('button').textContent = 'Failed';
    };
    box.appendChild(row);
  }
}

$('#send').onclick = async () => {
  const url = $('#url').value.trim();
  if (!url) return;
  const reply = await chrome.runtime.sendMessage({ type: 'takeover-request', url, referer: '' });
  $('#note').textContent = reply && reply.ok
    ? (reply.result && reply.result.takeover
        ? 'ODM took the download.'
        : 'ODM rejected it: ' + ((reply.result && reply.result.reason) || 'not a download'))
    : 'Failed: ' + ((reply && reply.error) || 'unknown');
};

$('#url').addEventListener('keydown', e => { if (e.key === 'Enter') $('#send').click(); });

$('#enabled').addEventListener('change', e => {
  chrome.runtime.sendMessage({ type: 'set-enabled', enabled: e.target.checked });
});

(async () => {
  await loadEnabled();
  await checkService();
  await scanMedia();
})();
