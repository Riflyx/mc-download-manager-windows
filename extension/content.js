// MC Download Manager — content script
// 1) In Safari (no downloads API) intercepts clicks on file links.
// 2) Reports <video>/<audio>/<source> URLs on the page to the background script.
(() => {
  'use strict';
  const api = typeof browser !== 'undefined' ? browser : chrome;
  const DL_EXT = /\.(zip|rar|7z|tar|gz|tgz|bz2|xz|dmg|pkg|iso|exe|msi|apk|deb|rpm|pdf|epub|mobi|doc|docx|xls|xlsx|ppt|pptx|mp4|mkv|avi|mov|webm|mp3|flac|wav|m4a|aac|ogg|jar|bin|img|csv|psd|ai|ttf|otf|woff2?)(\?|#|$)/i;

  // Chrome/Edge/Brave have the downloads API, so the background script intercepts downloads itself.
  // Only Safari (no downloads API) needs click interception and the periodic status refresh, which
  // otherwise wakes the service worker and pings the app every 30 s from every frame of every tab.
  const NEEDS_CLICK_INTERCEPT = typeof browser !== 'undefined' && !(typeof chrome !== 'undefined' && chrome.downloads);

  let state = { online: false, clickIntercept: false, settings: { intercept: true } };

  if (NEEDS_CLICK_INTERCEPT) {
  function refresh() {
    try {
      api.runtime.sendMessage({ type: 'status' }, (r) => {
        if (api.runtime.lastError || !r) return;
        state = r;
      });
    } catch {}
  }
  refresh();
  setInterval(refresh, 30000);

  document.addEventListener('click', (e) => {
    if (!state.clickIntercept || !state.online || !state.settings?.intercept) return;
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;
    const a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a) return;
    const href = a.href;
    if (!/^https?:/i.test(href)) return;
    const list = state.captureExtensions || [];
    const m = href.split(/[?#]/)[0].match(/\.([a-z0-9]{1,8})$/i);
    const ext = m ? m[1].toLowerCase() : '';
    if (list.length ? !(ext && list.includes(ext)) : !(a.hasAttribute('download') || DL_EXT.test(href))) return;
    e.preventDefault();
    e.stopPropagation();
    api.runtime.sendMessage({ type: 'download', url: href, filename: a.getAttribute('download') || '', referer: location.href, kind: 'file' });
  }, true);
  }

  const reported = new Set();
  function scanMedia() {
    const items = [];
    document.querySelectorAll('video, audio, source').forEach((el) => {
      const src = el.currentSrc || el.src || el.getAttribute('src');
      if (!src || !/^https?:/i.test(src) || reported.has(src)) return;
      reported.add(src);
      items.push({ url: src, type: el.getAttribute('type') || '', name: document.title ? document.title.slice(0, 80) : '' });
    });
    if (items.length) {
      try { api.runtime.sendMessage({ type: 'pageMedia', items }); } catch {}
    }
  }
  scanMedia();
  document.addEventListener('play', scanMedia, true);
  document.addEventListener('loadedmetadata', scanMedia, true);
  // A few delayed scans for players that attach late (play/loadedmetadata cover the rest);
  // previously this polled every 3 s for a minute in every frame.
  [2000, 6000, 15000].forEach((ms) => setTimeout(scanMedia, ms));
})();

// ---------------------------------------------------------------------------------------------
// Social-media integration: find the post a video belongs to (feeds show many posts on one URL,
// e.g. x.com/home), and an IDM-style download button on hovered videos.
// Lightweight by design: one delegated pointerover listener, no MutationObserver, no polling;
// the button is created lazily inside a closed shadow root so page CSS cannot touch it.
(() => {
  'use strict';
  const api = typeof browser !== 'undefined' ? browser : chrome;
  const t = (k) => { try { return api.i18n.getMessage(k) || k; } catch { return k; } };
  const H = location.hostname.replace(/^(www|m|mobile|mbasic)\./, '');
  const on = (re) => re.test(H);
  const abs = (h) => { try { return h ? new URL(h, location.href).href : ''; } catch { return ''; } };
  const closestAll = (el, sel) => (el && el.closest ? el.closest(sel) : null);
  const firstHref = (root, sel) => { const a = root && root.querySelector(sel); return a ? abs(a.getAttribute('href')) : ''; };

  // Returns the URL yt-dlp should get for the video `el` (or the page URL when nothing better is known).
  function postURLFor(el) {
    const path = location.pathname;
    try {
      if (on(/^(x|twitter)\.com$/)) {
        const art = closestAll(el, 'article');
        if (art) {
          const time = art.querySelector('a[href*="/status/"] time');
          const a = time ? time.closest('a') : art.querySelector('a[href*="/status/"]');
          if (a) return abs(a.getAttribute('href').replace(/\/(photo|video|analytics|history)\/.*$/, ''));
        }
        if (/\/status\/\d+/.test(path)) return location.href;
      } else if (on(/^instagram\.com$/)) {
        const art = closestAll(el, 'article') || closestAll(el, 'div[role="dialog"]');
        const u = firstHref(art, 'a[href*="/reel/"], a[href*="/p/"], a[href*="/tv/"]');
        if (u) return u;
        const m = path.match(/^\/(reels?|p|tv)\/([^/]+)/);
        if (m) return `https://www.instagram.com/${m[1] === 'reels' ? 'reel' : m[1]}/${m[2]}/`;
      } else if (on(/^tiktok\.com$/)) {
        const item = closestAll(el, '[data-e2e="recommend-list-item-container"], [data-e2e*="feed-item"], article, [class*="DivItemContainer"]');
        const u = firstHref(item, 'a[href*="/video/"], a[href*="/photo/"]');
        if (u) return u;
        const wrap = closestAll(el, '[id^="xgwrapper-"]');
        const id = wrap && (wrap.id.match(/(\d{15,})$/) || [])[1];
        if (id) {
          const user = (firstHref(item, 'a[href^="/@"]').match(/\/@([^/?]+)/) || [])[1] || 'i';
          return `https://www.tiktok.com/@${user}/video/${id}`;
        }
      } else if (on(/(^|\.)youtube\.com$/)) {
        if (/^\/(watch|shorts\/|live\/|clip\/)/.test(path)) return location.href;
        const prev = closestAll(el, 'ytd-video-preview, #video-preview');
        const u = firstHref(prev, 'a[href*="/watch"], a[href*="/shorts/"]') ||
          firstHref(closestAll(el, 'ytd-rich-item-renderer, ytd-video-renderer, ytd-compact-video-renderer, ytd-grid-video-renderer, ytd-reel-item-renderer, ytm-shorts-lockup-view-model, yt-lockup-view-model'),
            'a[href*="/watch"], a[href*="/shorts/"]');
        if (u) return u;
      } else if (on(/(^|\.)reddit\.com$/)) {
        const p = closestAll(el, 'shreddit-post');
        if (p && p.getAttribute('permalink')) return abs(p.getAttribute('permalink'));
        const thing = closestAll(el, '.thing[data-permalink]');
        if (thing) return abs(thing.getAttribute('data-permalink'));
      } else if (on(/(^|\.)facebook\.com$/)) {
        const art = closestAll(el, '[role="article"], [data-pagelet*="FeedUnit"]');
        const u = firstHref(art, 'a[href*="/videos/"], a[href*="/reel/"], a[href*="/watch/?v="], a[href*="/watch?v="]');
        if (u) return u.split('&__')[0];
      } else if (on(/^bsky\.app$/)) {
        const u = firstHref(closestAll(el, '[data-testid^="feedItem"], [data-testid^="postThreadItem"]'), 'a[href*="/post/"]');
        if (u) return u;
      } else if (on(/^threads\.(net|com)$/)) {
        const u = firstHref(closestAll(el, '[data-pressable-container]'), 'a[href*="/post/"]');
        if (u) return u;
      } else if (on(/(^|\.)linkedin\.com$/)) {
        const post = closestAll(el, '[data-urn*="activity:"]');
        const urn = post && post.getAttribute('data-urn');
        if (urn) return `https://www.linkedin.com/feed/update/${urn}/`;
      } else if (on(/(^|\.)twitch\.tv$/)) {
        const u = firstHref(closestAll(el, 'article, [data-a-target*="card"]'), 'a[href*="/videos/"], a[href*="/clip/"]');
        if (u) return u;
      }
    } catch {}
    // Generic: a directly playable file is better than the page.
    const v = el && el.tagName === 'VIDEO' ? el : null;
    const src = v && (v.currentSrc || v.src);
    if (src && /^https?:/i.test(src) && /\.(mp4|webm|m4v|mov)(\?|#|$)/i.test(src)) return src;
    return location.href;
  }

  // ---- context menu: remember what was right-clicked; background asks for the post URL ----
  let lastCtx = null;
  document.addEventListener('contextmenu', (e) => { lastCtx = e.target; }, true);
  api.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
    if (msg && msg.type === 'resolveTarget') {
      let el = lastCtx;
      if (el && el.tagName !== 'VIDEO') el = findVideoNear(el, msg.x, msg.y) || el;
      sendResponse({ url: postURLFor(el), page: location.href });
    }
  });

  // ---- hover button ----
  // Top-level site (iframes are judged by the page that embeds them).
  const topHost = (() => {
    try { const a = location.ancestorOrigins; return new URL(a && a.length ? a[a.length - 1] : location.href).hostname.toLowerCase().replace(/^www\./, ''); } catch { return H; }
  })();
  const allowed = (st) => !!st && st.overlay !== false &&
    !(st.disabledSites || []).some((s) => topHost === s || topHost.endsWith('.' + s));
  let enabled = true;
  try {
    api.storage.local.get('settings').then((s) => { enabled = allowed({ overlay: true, ...(s.settings || {}) }); });
    api.storage.onChanged.addListener((c) => { if (c.settings) { enabled = allowed({ overlay: true, ...(c.settings.newValue || {}) }); if (!enabled) hide(); } });
  } catch {}

  const MIN_W = 200, MIN_H = 112;
  let host = null, btn = null, current = null, hideTimer = null;

  function findVideoNear(el, x, y) {
    // Sites stack overlays (X, Instagram…) far above the <video> in the DOM, so hit-test by position instead:
    // any sufficiently large video whose box contains the pointer.
    if (el && el.tagName === 'VIDEO') return el;
    const vids = document.querySelectorAll('video');
    if (vids.length > 40) return null;
    for (const v of vids) {
      const r = v.getBoundingClientRect();
      if (r.width < MIN_W || r.height < MIN_H) continue;
      if (x == null || (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom)) return v;
    }
    return null;
  }

  function ensureButton() {
    if (host) return;
    host = document.createElement('mcdm-dl');
    host.style.cssText = 'all:initial;position:fixed;z-index:2147483647;top:0;left:0;display:none;transform:translateX(-100%)';
    const root = host.attachShadow({ mode: 'closed' });
    root.innerHTML = `<style>
      button{all:initial;cursor:pointer;display:flex;align-items:center;gap:6px;padding:9px;border-radius:10px;
        background:rgba(20,22,28,.82);color:#fff;font:600 12.5px/1 "Segoe UI",system-ui,sans-serif;box-shadow:0 2px 10px rgba(0,0,0,.35);
        backdrop-filter:blur(6px);transition:background .15s,transform .1s}
      button:hover{background:#2f6feb}button:active{transform:scale(.96)}
      svg{width:18px;height:18px;flex:none}span{display:none;padding-right:3px}.fb span{display:inline}</style>
      <button title=""><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M12 4v11m0 0l-4.5-4.5M12 15l4.5-4.5M5 20h14"/></svg><span></span></button>`;
    btn = root.querySelector('button');
    
    btn.title = t('menuPageVideo');
    btn.addEventListener('click', onClick, true);
    btn.addEventListener('pointerdown', (e) => e.stopPropagation(), true);
    host.addEventListener('pointerenter', () => clearTimeout(hideTimer));
    (document.documentElement || document.body).appendChild(host);
  }

  function place(v) {
    const r = v.getBoundingClientRect();
    if (r.width < MIN_W || r.height < MIN_H || r.bottom < 0 || r.top > innerHeight) { hide(); return; }
    ensureButton();
    host.style.display = 'block';
    host.style.top = Math.max(4, r.top + 10) + 'px';
    const shift = on(/^instagram\.com$/) ? 110 : 0; // Instagram puts its own icons (and other extensions') in the corner
    host.style.left = Math.max(120, Math.min(innerWidth - 6, r.right - 10 - shift)) + 'px'; // top-right, grows leftwards
  }

  function hide() { if (host) host.style.display = 'none'; current = null; }

  async function onClick(e) {
    e.preventDefault(); e.stopPropagation();
    if (!current) return;
    const url = postURLFor(current);
    const label = btn.querySelector('span');
    label.textContent = '…'; btn.classList.add('fb');
    let r = null;
    try { r = await api.runtime.sendMessage({ type: 'download', url, referer: location.href, kind: /\.(mp4|webm|m4v|mov)(\?|#|$)/i.test(url) && url !== location.href ? 'file' : 'video' }); } catch {}
    label.textContent = r && r.ok ? '✓ ' + t('popupSent') : t('popupFailed');
    setTimeout(() => { btn.classList.remove('fb'); label.textContent = ''; }, 2200);
  }

  let lastMove = 0;
  const onPointer = (e) => {
    if (e.type === 'pointermove') { const n = Date.now(); if (n - lastMove < 100) return; lastMove = n; }
    if (!enabled || e.pointerType === 'touch') return;
    if (host && e.composedPath().includes(host)) return;
    const v = findVideoNear(e.target, e.clientX, e.clientY);
    if (v) {
      clearTimeout(hideTimer);
      current = v;
      place(v);
    } else if (current) {
      clearTimeout(hideTimer);
      hideTimer = setTimeout(hide, 350);
    }
  };
  document.addEventListener('pointerover', onPointer, { capture: true, passive: true });
  document.addEventListener('pointermove', onPointer, { capture: true, passive: true });
  let raf = 0;
  addEventListener('scroll', () => { if (current && !raf) raf = requestAnimationFrame(() => { raf = 0; if (current) place(current); }); }, { capture: true, passive: true });
  addEventListener('resize', () => { if (current) place(current); }, { passive: true });
  document.addEventListener('fullscreenchange', hide);
})();

// ---------------------------------------------------------------------------------------------
// Toast in the page corner: confirms that a download went to the app and shows its progress.
// Only built when a download is sent; nothing runs on pages otherwise.
(() => {
  'use strict';
  if (window.top !== window) return;
  const api = typeof browser !== 'undefined' ? browser : chrome;
  const t = (k) => { try { return api.i18n.getMessage(k) || k; } catch { return k; } };
  const fmt = (n) => { if (!(n > 0)) return ''; const u = ['B', 'KB', 'MB', 'GB']; let i = 0; while (n >= 1024 && i < 3) { n /= 1024; i++; } return n.toFixed(i ? 1 : 0) + ' ' + u[i]; };
  let host = null, root = null, list = null;
  const cards = new Map(); // id -> {el, timer}

  function ensure() {
    if (host && host.isConnected) return;
    host = document.createElement('mcdm-toast');
    host.style.cssText = 'all:initial;position:fixed;z-index:2147483647;right:16px;bottom:16px;pointer-events:none';
    root = host.attachShadow({ mode: 'closed' });
    root.innerHTML = `<style>
      .wrap{display:flex;flex-direction:column;gap:8px;align-items:flex-end;font:13px/1.35 "Segoe UI",system-ui,sans-serif}
      .card{pointer-events:auto;width:320px;box-sizing:border-box;background:rgba(22,24,30,.96);color:#e8eaee;border:1px solid rgba(255,255,255,.08);
        border-radius:12px;padding:10px 12px 10px 12px;box-shadow:0 8px 28px rgba(0,0,0,.35);display:grid;grid-template-columns:22px 1fr auto;gap:2px 10px;
        align-items:center;animation:in .18s ease-out}
      @keyframes in{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:none}}
      .ic{grid-column:1;grid-row:1/3;width:22px;height:22px;border-radius:6px;background:#2f6feb;display:grid;place-items:center;font-size:13px;color:#fff}
      .card.ok .ic{background:#1f9d55}.card.bad .ic{background:#d1343b}
      .t{grid-column:2;grid-row:1;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
      .s{grid-column:2;grid-row:2;color:#9aa3b2;font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
      .x{all:unset;cursor:pointer;color:#7d8696;font-size:15px;padding:0 2px;grid-column:3;grid-row:1;justify-self:end}
      .x:hover{color:#fff}
      .a{all:unset;cursor:pointer;color:#7fa7ff;font-size:12px;grid-column:3;grid-row:2;justify-self:end}
      .a:hover{text-decoration:underline}
      .bar{grid-column:2/4;height:3px;border-radius:3px;background:rgba(255,255,255,.1);overflow:hidden;margin-top:6px}
      .bar>i{display:block;height:100%;width:0;background:#2f6feb;transition:width .5s}
      .bar.ind>i{width:30%;animation:ind 1.2s infinite ease-in-out}
      @keyframes ind{0%{margin-left:-30%}100%{margin-left:100%}}
      </style><div class="wrap"></div>`;
    list = root.querySelector('.wrap');
    (document.body || document.documentElement).appendChild(host);
  }

  function render(d) {
    ensure();
    let c = cards.get(d.id);
    if (!c) {
      const el = document.createElement('div');
      el.className = 'card';
      el.innerHTML = '<div class="ic">↓</div><div class="t"></div><button class="x" title="×">×</button><div class="s"></div><button class="a"></button><div class="bar"><i></i></div>';
      el.querySelector('.x').onclick = () => drop(d.id);
      for (const q of ['.ic', '.t', '.s']) { const n = el.querySelector(q); n.style.cursor = 'pointer'; n.onclick = () => { try { api.runtime.sendMessage({ type: 'openUI' }); } catch {} }; }
      list.appendChild(el);
      c = { el, timer: 0 };
      cards.set(d.id, c);
      while (cards.size > 3) drop(cards.keys().next().value);
    }
    const el = c.el, st = d.state;
    const done = st === 'completed' || st === 'handedBack', bad = st === 'failed' || st === 'cancelled' || st === 'offline';
    el.classList.toggle('ok', done); el.classList.toggle('bad', bad);
    el.querySelector('.ic').textContent = done ? '✓' : bad ? '!' : '↓';
    el.querySelector('.t').textContent = d.name || '';
    let sub;
    if (st === 'offline') sub = t('notifOfflineTitle');
    else if (st === 'handedBack') sub = t('toastHandedBack');
    else if (d.duplicate === 'done') sub = t('toastAlreadyDone');
    else if (done) sub = t('stateCompleted') + (d.total > 0 ? ' · ' + fmt(d.total) : '');
    else if (bad) sub = t(st === 'cancelled' ? 'stateCancelled' : 'stateFailed');
    else if (d.duplicate === 'active' && !(d.received > 0)) sub = t('toastAlreadyActive');
    else if (d.received > 0 && d.total > 0) sub = `${Math.floor((100 * d.received) / d.total)}% · ${fmt(d.received)} / ${fmt(d.total)}` + (d.speed > 0 ? ` · ${fmt(d.speed)}/s` : '');
    else if (d.stage) sub = d.stage;
    else sub = t(d.kind === 'video' ? 'notifVideoTitle' : 'notifSentTitle');
    el.querySelector('.s').textContent = sub;
    const bar = el.querySelector('.bar');
    bar.style.display = done || bad ? 'none' : '';
    const pct = d.total > 0 ? Math.min(100, (100 * (d.received || 0)) / d.total) : 0;
    bar.classList.toggle('ind', !(pct > 0));
    bar.querySelector('i').style.width = pct + '%';
    const a = el.querySelector('.a');
    if (done && d.id !== 'err' && st !== 'handedBack') {
      a.textContent = t('toastReveal'); a.classList.remove('hidden'); a.style.display = '';
      a.onclick = () => { try { api.runtime.sendMessage({ type: 'reveal', id: d.id }); } catch {} };
    } else a.style.display = 'none';
    clearTimeout(c.timer);
    if (done || bad) c.timer = setTimeout(() => drop(d.id), done ? 6000 : 8000);
    else c.timer = setTimeout(() => drop(d.id), 120000); // stale guard
  }

  function drop(id) {
    const c = cards.get(id);
    if (!c) return;
    clearTimeout(c.timer); c.el.remove(); cards.delete(id);
    if (!cards.size && host) { host.remove(); host = null; }
  }

  api.runtime.onMessage.addListener((msg, _s, sendResponse) => {
    if (msg && msg.type === 'dlToast') { try { render(msg); sendResponse({ ok: true }); } catch { sendResponse({ ok: false }); } }
  });
})();
