'use strict';

/* ══════════════════════════════════════════════════════════════════════════
   ODM content script.

   Two jobs, both mirroring the shipped IDM integration module:
     1. show a small "Download this link with ODM" affordance on hover for
        links whose target looks like a file;
     2. re-post the document's media URLs (video/audio) so the popup can offer
        "download this page's media", the same way IDM shows a media panel.
   ══════════════════════════════════════════════════════════════════════════ */

(function () {
  if (window.__odmInjected) return;
  window.__odmInjected = true;

  const FILE_RE = /\.(zip|rar|7z|gz|tar|iso|bz2|xz|exe|msi|msp|apk|dmg|deb|pdf|docx?|xlsx?|pptx?|epub|mp4|mkv|avi|mov|flv|webm|m4v|mp3|m4a|flac|aac|ogg|wav|png|jpe?g|gif|webp|svg|txt|csv)(\?|$)/i;

  let tip = null;

  function ensureTip() {
    if (tip) return tip;
    tip = document.createElement('button');
    tip.type = 'button';
    tip.textContent = '⤓ ODM';
    tip.style.cssText = [
      'position:fixed', 'z-index:2147483647', 'padding:3px 8px',
      'font:600 11px/1.4 "Segoe UI",sans-serif', 'color:#fff',
      'background:linear-gradient(180deg,#4dabf7,#1f6feb)',
      'border:1px solid #1657bd', 'border-radius:4px',
      'box-shadow:0 2px 8px rgba(0,0,0,.28)', 'cursor:pointer',
      'display:none', 'user-select:none',
    ].join(';');
    tip.addEventListener('mousedown', e => {
      e.preventDefault();
      e.stopPropagation();
      const url = tip.dataset.url;
      if (url) {
        chrome.runtime.sendMessage(
          { type: 'takeover-request', url, referer: location.href },
          reply => {
            if (reply && reply.ok) flash('ODM started the download');
            else flash('ODM: ' + ((reply && reply.error) || 'failed'));
          }
        );
      }
      hideTip();
    });
    document.documentElement.appendChild(tip);
    return tip;
  }

  function flash(msg) {
    const n = document.createElement('div');
    n.textContent = msg;
    n.style.cssText = [
      'position:fixed', 'right:16px', 'bottom:16px', 'z-index:2147483647',
      'padding:8px 14px', 'font:600 12px "Segoe UI",sans-serif', 'color:#fff',
      'background:#1e2229', 'border-radius:6px', 'box-shadow:0 6px 20px rgba(0,0,0,.3)',
      'opacity:0', 'transition:opacity .18s',
    ].join(';');
    document.documentElement.appendChild(n);
    requestAnimationFrame(() => { n.style.opacity = '1'; });
    setTimeout(() => {
      n.style.opacity = '0';
      setTimeout(() => n.remove(), 220);
    }, 1600);
  }

  function showTip(anchor, url) {
    const t = ensureTip();
    const r = anchor.getBoundingClientRect();
    t.dataset.url = url;
    t.style.left = Math.min(window.innerWidth - 80, Math.max(4, r.left)) + 'px';
    t.style.top = Math.min(window.innerHeight - 30, r.bottom + 4) + 'px';
    t.style.display = 'block';
  }

  function hideTip() { if (tip) tip.style.display = 'none'; }

  document.addEventListener('mouseover', e => {
    const a = e.target instanceof Element ? e.target.closest('a[href]') : null;
    if (!a) return;
    const href = a.href;
    if (!/^https?:/i.test(href)) return;
    if (!FILE_RE.test(href)) return;
    showTip(a, href);
  }, true);

  document.addEventListener('mouseout', e => {
    if (e.target instanceof Element && e.target.closest('a[href]')) hideTip();
  }, true);

  window.addEventListener('scroll', hideTip, { passive: true });

  /* ── media discovery for the popup ─────────────────────────────────── */

  function collectMedia() {
    const out = [];
    const push = (u, kind) => {
      if (!u || !/^https?:/i.test(u)) return;
      if (out.some(m => m.url === u)) return;
      out.push({ url: u, kind });
    };
    for (const v of document.querySelectorAll('video, audio')) {
      push(v.currentSrc || v.src, v.tagName.toLowerCase());
      for (const s of v.querySelectorAll('source[src]')) push(s.src, v.tagName.toLowerCase());
    }
    for (const s of document.querySelectorAll('source[src]')) push(s.src, 'source');
    for (const l of document.querySelectorAll('a[href]')) {
      if (FILE_RE.test(l.href)) push(l.href, 'link');
    }
    for (const el of document.querySelectorAll('[data-src],[data-url],[data-hls],[data-mp4]')) {
      for (const attr of ['data-src', 'data-url', 'data-hls', 'data-mp4']) {
        const v = el.getAttribute(attr);
        if (v) push(new URL(v, location.href).href, 'data');
      }
    }
    return out.slice(0, 60);
  }

  chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
    if (msg.type === 'scan-media') {
      reply({ ok: true, page: { url: location.href, title: document.title }, media: collectMedia() });
      return true;
    }
    return false;
  });
})();
