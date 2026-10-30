// MC Download Manager bridge extension — background script (Chrome MV3 / Safari)
'use strict';

const api = typeof browser !== 'undefined' ? browser : chrome;
const t = (k, subs) => { try { return api.i18n.getMessage(k, subs) || k; } catch { return k; } };
const IS_SAFARI = typeof browser !== 'undefined' && !!browser.runtime.sendNativeMessage && !chrome.downloads?.onDeterminingFilename;

const DEFAULTS = { port: 48213, intercept: true, minSizeMB: 0, sniff: true, overlay: true, disabledSites: [] };

// ---------- Per-site off switch ----------
function siteOf(url) {
  try { return new URL(url).hostname.toLowerCase().replace(/^www\./, ''); } catch { return ''; }
}
function siteDisabled(settings, url) {
  const h = siteOf(url);
  if (!h) return false;
  return (settings.disabledSites || []).some((s) => h === s || h.endsWith('.' + s));
}
const BROWSER = IS_SAFARI ? 'safari' : (navigator.brave ? 'brave' : (/Edg\//.test(navigator.userAgent) ? 'edge' : 'chrome'));

const MEDIA_EXT = /\.(mp4|m4v|webm|mkv|mov|avi|flv|mp3|m4a|aac|ogg|opus|wav|flac)(\?|#|$)/i;
const FRAGMENT = /\.(ts|m4s|m4f|cmfv|cmfa|fmp4|init)(\?|#|$)|[/_.-](seg|segment|chunk|frag|fragment)[-_]?\d+/i;
const IGNORED_HOSTS = /(^|\.)googlevideo\.com$|(^|\.)fbcdn\.net$|(^|\.)twimg\.com$/i;

// ---------- Settings ----------
async function getSettings() {
  const s = await api.storage.local.get('settings');
  return { ...DEFAULTS, ...(s.settings || {}) };
}
async function setSettings(patch) {
  const cur = await getSettings();
  await api.storage.local.set({ settings: { ...cur, ...patch } });
}
const base = (s) => `http://127.0.0.1:${s.port}`;
// Custom header = proof the request comes from an extension: ordinary web pages cannot send it
// without a CORS preflight, and the bridge only grants CORS to extension origins. Chrome omits the
// Origin header on extension GET requests that are covered by host permissions, so Origin alone is not enough.
const BRIDGE_HEADERS = { 'X-MCDM-Extension': BROWSER, 'X-MCDM-Version': api.runtime.getManifest().version };

// ---------- App communication ----------
async function ping(settings) {
  settings = settings || (await getSettings());
  try {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), 1200);
    const r = await fetch(base(settings) + '/ping?browser=' + BROWSER, { signal: ctrl.signal, cache: 'no-store', headers: BRIDGE_HEADERS });
    clearTimeout(t);
    if (!r.ok) return null;
    const app = await r.json();
    if (app && app.active > 0) startProgress();
    return app;
  } catch {
    return null;
  }
}

// ---------- Download progress: toolbar badge + popup list ----------
async function bridgeJSON(path, init) {
  const settings = await getSettings();
  const r = await fetch(base(settings) + path, { cache: 'no-store', ...init, headers: { ...BRIDGE_HEADERS, ...(init?.headers || {}) } });
  return r.json();
}
async function getDownloads() {
  try { const j = await bridgeJSON('/downloads'); return (j && j.items) || []; } catch { return []; }
}
async function downloadAction(id, action) {
  try {
    const j = await bridgeJSON('/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id, action }) });
    tickProgress();
    return j;
  } catch (e) { return { ok: false, error: String(e) }; }
}
const ACTIVE_STATES = new Set(['downloading', 'probing']);
let progressTimer = null;
let badgeShown = false;
async function setGlobalBadge(text, color) {
  try { await api.action.setBadgeText({ text }); if (color) await api.action.setBadgeBackgroundColor({ color }); } catch {}
}
// Per-tab media counts override the global badge; while downloads run, every tab falls back to the global text.
async function useGlobalBadgeOnAllTabs(on) {
  try {
    const tabs = await api.tabs.query({});
    for (const tab of tabs) {
      if (on) { try { await api.action.setBadgeText({ tabId: tab.id, text: null }); } catch {} }
      else { const list = await readMedia(tab.id); try { await api.action.setBadgeText({ tabId: tab.id, text: list.length ? String(list.length) : '' }); } catch {} }
    }
  } catch {}
}
async function tickProgress() {
  const list = await getDownloads();
  for (const it of list) {
    const hb = handBack.get(it.id);
    if (hb && it.state === 'failed' && /HTTP (401|403|410)\b/.test(it.error || '')) {
      handBack.delete(it.id); toastTabs.delete(it.id);
      await downloadAction(it.id, 'remove');
      try { await api.downloads.download({ url: hb.url, ...(hb.filename ? { filename: hb.filename } : {}) }); } catch {}
      pushToast(hb.tabId, { id: it.id, name: hb.filename || it.name, state: 'handedBack' });
      continue;
    }
    if (hb && ['completed', 'failed', 'cancelled'].includes(it.state)) handBack.delete(it.id);
    const tabId = toastTabs.get(it.id);
    if (tabId == null) continue;
    pushToast(tabId, it);
    if (['completed', 'failed', 'cancelled'].includes(it.state)) toastTabs.delete(it.id);
  }
  const active = list.filter((i) => ACTIVE_STATES.has(i.state));
  if (active.length) {
    const measurable = active.filter((i) => i.total > 0);
    let text = '…';
    if (active.length > 1) text = active.length + '↓';
    else if (measurable.length) {
      const total = measurable.reduce((a, i) => a + i.total, 0);
      const got = measurable.reduce((a, i) => a + Math.min(i.received, i.total), 0);
      text = Math.round((100 * got) / total) + '%';
    }
    if (!badgeShown) { badgeShown = true; await useGlobalBadgeOnAllTabs(true); }
    await setGlobalBadge(text, '#2f6feb');
  } else {
    stopProgress();
    if (badgeShown) {
      badgeShown = false;
      await setGlobalBadge('✓', '#2ebd59');
      setTimeout(async () => { await setGlobalBadge('', '#2f6feb'); await useGlobalBadgeOnAllTabs(false); }, 4000);
    }
  }
}
function startProgress() {
  if (progressTimer) return;
  tickProgress();
  progressTimer = setInterval(tickProgress, 1500);  // each fetch keeps the MV3 worker alive while something runs
}
function stopProgress() {
  if (progressTimer) { clearInterval(progressTimer); progressTimer = null; }
}

async function cookiesFor(url) {
  try {
    const list = await api.cookies.getAll({ url });
    return {
      header: list.map((c) => `${c.name}=${c.value}`).join('; '),
      list: list.map((c) => ({
        name: c.name, value: c.value, domain: c.domain, path: c.path,
        secure: !!c.secure, expirationDate: c.expirationDate || 0,
      })),
    };
  } catch {
    return { header: '', list: [] };
  }
}

function notify(title, message) {
  try {
    api.notifications?.create?.('', {
      type: 'basic', iconUrl: 'icons/icon128.png', title, message: String(message || '').slice(0, 200),
    });
  } catch {}
}

// ---------- In-page toast: "sent to the app" + live progress in the tab the download came from ----------
const toastTabs = new Map(); // download id -> tabId
// Browser downloads handed to the app: if the app is refused (Cloudflare, single-use signed links such as
// HLTV demos), give the download back to the browser, which re-opens the original link with its own session.
const handBack = new Map(); // download id -> { url, filename, tabId }
async function activeTabId() {
  try { const [tb] = await api.tabs.query({ active: true, lastFocusedWindow: true }); return tb && /^https?:/.test(tb.url || '') ? tb.id : null; } catch { return null; }
}
async function pushToast(tabId, data) {
  if (tabId == null) return false;
  try { const r = await api.tabs.sendMessage(tabId, { type: 'dlToast', ...data }, { frameId: 0 }); return !!(r && r.ok); } catch { return false; }
}

async function sendToApp({ url, filename, referer, kind, sourceUrl, tabId }) {
  const settings = await getSettings();
  const ck = await cookiesFor(url);
  const payload = {
    url, filename: filename || '', referer: referer || '', kind: kind || 'file', sourceUrl: sourceUrl || '',
    userAgent: navigator.userAgent, cookieHeader: ck.header, cookies: ck.list,
  };
  if (tabId === undefined) tabId = await activeTabId();
  try {
    const r = await fetch(base(settings) + '/download', {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...BRIDGE_HEADERS }, body: JSON.stringify(payload),
    });
    const j = await r.json();
    if (!j.ok) throw new Error(j.error || 'app error');
    const name = j.name || filename || url;
    const shown = await pushToast(tabId, { id: j.id, name, kind: kind || 'file', state: j.duplicate === 'done' ? 'completed' : 'queued', duplicate: j.duplicate || '' });
    if (shown && j.duplicate !== 'done') toastTabs.set(j.id, tabId);
    if (!shown) notify(kind === 'video' ? t('notifVideoTitle') : t('notifSentTitle'), name);   // e.g. no content script on this tab
    startProgress();
    return { ok: true, id: j.id, duplicate: j.duplicate || '' };
  } catch (e) {
    if (!(await pushToast(tabId, { id: 'err', name: filename || url, state: 'offline' }))) notify(t('notifOfflineTitle'), t('notifOfflineBody'));
    return { ok: false, error: String(e) };
  }
}

// Safari: launch the app through native messaging
async function launchApp() {
  if (!api.runtime.sendNativeMessage) return false;
  try {
    const r = await api.runtime.sendNativeMessage('application.id', { action: 'launchApp' });
    return !!(r && r.ok);
  } catch {
    return false;
  }
}

// ---------- Capture file-type filter (list comes from the app) ----------
function extOf(name) {
  const m = String(name || '').split(/[?#]/)[0].match(/\.([a-z0-9]{1,8})$/i);
  return m ? m[1].toLowerCase() : '';
}
function matchesCaptureTypes(app, filename, url) {
  const list = app && Array.isArray(app.captureExtensions) ? app.captureExtensions : [];
  if (!list.length) return true;
  let path = '';
  try { path = new URL(url).pathname; } catch {}
  const e = extOf(filename) || extOf(path);
  return !!e && list.includes(e);
}

// ---------- Download interception (Chrome) ----------
async function interceptDownload(item) {
  const settings = await getSettings();
  if (!settings.intercept) return false;
  const url = item.finalUrl || item.url;
  if (!/^https?:/i.test(url)) return false;
  if (item.byExtensionId) return false;
  if (settings.minSizeMB > 0 && item.totalBytes > 0 && item.totalBytes < settings.minSizeMB * 1024 * 1024) return false;
  if (siteDisabled(settings, item.referrer || url) || siteDisabled(settings, url)) return false;
  const app = await ping(settings);
  if (!app) return false;
  if (!matchesCaptureTypes(app, item.filename || '', url)) return false;

  try { await api.downloads.cancel(item.id); } catch {}
  try { await api.downloads.erase({ id: item.id }); } catch {}
  const filename = item.filename ? item.filename.split(/[\\/]/).pop() : '';
  const r = await sendToApp({ url, filename, referer: item.referrer, kind: 'file', sourceUrl: item.url !== url ? item.url : '' });
  if (r.ok && r.duplicate !== 'done') handBack.set(r.id, { url: item.url, filename, tabId: await activeTabId() });
  if (!r.ok) {
    // The app could not take it after we cancelled the browser download: hand it back to the browser
    // (byExtensionId is set on this one, so it is not intercepted again).
    try { await api.downloads.download({ url: item.url, ...(filename ? { filename } : {}) }); } catch {}
  }
  return true;
}

if (api.downloads?.onDeterminingFilename) {
  api.downloads.onDeterminingFilename.addListener((item, suggest) => {
    // suggest() must always be called, otherwise the browser download hangs forever.
    interceptDownload(item).then((took) => { if (!took) { try { suggest(); } catch {} } }, () => { try { suggest(); } catch {} });
    return true;
  });
} else if (api.downloads?.onCreated) {
  api.downloads.onCreated.addListener((item) => { interceptDownload(item); });
}

// ---------- Media sniffing ----------
const mem = new Map(); // tabId -> Map(url -> info)   (fallback when storage.session is unavailable)

async function readMedia(tabId) {
  if (api.storage.session) {
    const key = 'media:' + tabId;
    const r = await api.storage.session.get(key);
    return r[key] || [];
  }
  return Array.from((mem.get(tabId) || new Map()).values());
}
async function writeMedia(tabId, list) {
  if (api.storage.session) {
    await api.storage.session.set({ ['media:' + tabId]: list });
  } else {
    mem.set(tabId, new Map(list.map((m) => [m.url, m])));
  }
  try {
    await api.action.setBadgeText({ tabId, text: list.length ? String(list.length) : '' });
    await api.action.setBadgeBackgroundColor({ tabId, color: '#2f6feb' });
  } catch {}
}
async function clearMedia(tabId) {
  if (api.storage.session) await api.storage.session.remove('media:' + tabId);
  mem.delete(tabId);
  try { await api.action.setBadgeText({ tabId, text: '' }); } catch {}
}

function normalizeKey(url) {
  try {
    const u = new URL(url);
    ['range', 'bytes', 'start', 'end', 'rn', 'rbuf', 'r', '_'].forEach((p) => u.searchParams.delete(p));
    return u.origin + u.pathname + (u.searchParams.toString() ? '?' + u.searchParams.toString() : '');
  } catch {
    return url;
  }
}

async function addMedia(tabId, info) {
  if (tabId < 0) return;
  const settings = await getSettings();
  if (!settings.sniff) return;
  if ((settings.disabledSites || []).length) {
    try { const tab = await api.tabs.get(tabId); if (siteDisabled(settings, tab.url)) return; } catch {}
  }
  const list = await readMedia(tabId);
  const key = normalizeKey(info.url);
  const idx = list.findIndex((m) => m.key === key);
  const entry = { ...info, key, name: info.name || guessName(info.url, info.type) };
  if (idx >= 0) {
    if ((info.size || 0) > (list[idx].size || 0)) list[idx] = { ...list[idx], ...entry };
    return;
  }
  list.unshift(entry);
  if (list.length > 40) list.length = 40;
  await writeMedia(tabId, list);
}

function guessName(url, type) {
  try {
    const u = new URL(url);
    let n = decodeURIComponent(u.pathname.split('/').filter(Boolean).pop() || '') || u.hostname;
    if (!/\.[a-z0-9]{2,5}$/i.test(n) && type) {
      const ext = { 'video/mp4': 'mp4', 'video/webm': 'webm', 'audio/mpeg': 'mp3', 'audio/mp4': 'm4a', 'video/quicktime': 'mov' }[type.toLowerCase()];
      if (ext) n += '.' + ext;
    }
    return n;
  } catch {
    return url;
  }
}

function headerVal(headers, name) {
  const h = (headers || []).find((x) => x.name.toLowerCase() === name);
  return h ? h.value : '';
}
function classify(url, ct) {
  ct = (ct || '').toLowerCase();
  if (/\.m3u8(\?|#|$)/i.test(url) || /mpegurl/.test(ct)) return 'hls';
  if (/\.mpd(\?|#|$)/i.test(url) || /dash\+xml/.test(ct)) return 'dash';
  if (FRAGMENT.test(url)) return null;
  if (/^(video|audio)\//.test(ct)) {
    if (/mp2t|iso\.segment|x-mpegurl/.test(ct)) return null;
    return 'media';
  }
  if (/octet-stream/.test(ct) && MEDIA_EXT.test(url)) return 'media';
  return null;
}

if (api.webRequest?.onHeadersReceived) {
  api.webRequest.onHeadersReceived.addListener(
    (d) => {
      if (d.tabId < 0 || d.statusCode >= 400) return;
      let host = '';
      try { host = new URL(d.url).hostname; } catch {}
      if (IGNORED_HOSTS.test(host)) return;
      const ct = headerVal(d.responseHeaders, 'content-type');
      const kind = classify(d.url, ct);
      if (!kind) return;
      let size = parseInt(headerVal(d.responseHeaders, 'content-length'), 10) || 0;
      const cr = headerVal(d.responseHeaders, 'content-range').match(/\/(\d+)/);
      if (cr) size = parseInt(cr[1], 10);
      if (kind === 'media' && size > 0 && size < 300 * 1024) return;
      addMedia(d.tabId, { url: d.url, type: ct.split(';')[0], size, kind, time: Date.now() });
    },
    { urls: ['<all_urls>'], types: ['media', 'xmlhttprequest', 'other', 'object', 'main_frame'] },
    ['responseHeaders']
  );
}

api.tabs.onUpdated.addListener((tabId, info) => {
  if (info.status === 'loading' && info.url) clearMedia(tabId);
});
api.tabs.onRemoved.addListener((tabId) => clearMedia(tabId));

// ---------- Context menu ----------
const menus = api.contextMenus || api.menus;
function createMenus() {
  if (!menus) return;
  try {
    menus.removeAll(() => {
      menus.create({ id: 'dm-link', title: t('menuDownloadLink'), contexts: ['link', 'video', 'audio', 'image'] });
      menus.create({ id: 'dm-page-video', title: t('menuPageVideo'), contexts: ['page', 'video', 'frame', 'action'] });
      menus.create({ id: 'dm-settings', title: t('menuSettings'), contexts: ['action'] });
    });
  } catch {}
}
api.runtime.onInstalled.addListener(() => { createMenus(); ping(); });
api.runtime.onStartup?.addListener(() => { createMenus(); ping(); });
// Asks the content script in the clicked frame which post the right-clicked video belongs to
// (feeds like x.com/home show many posts under one URL).
async function resolveTarget(tab, frameId) {
  if (!tab || tab.id == null) return null;
  try {
    const r = await api.tabs.sendMessage(tab.id, { type: 'resolveTarget' }, { frameId: frameId || 0 });
    return r && r.url ? r.url : null;
  } catch { return null; }
}

menus?.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === 'dm-settings') {
    // The old popup (site switch, toggles, port) now lives in a tab; it needs to know which site it was opened for.
    const q = tab && tab.id != null ? `?tab=${tab.id}` : '';
    api.tabs.create({ url: api.runtime.getURL('popup.html') + q });
    return;
  }
  const referer = info.frameUrl || info.pageUrl || tab?.url;
  if (info.menuItemId === 'dm-link') {
    const src = info.srcUrl || '';
    // A <video> with a blob:/MSE source cannot be fetched as a file — download its post with yt-dlp.
    if (info.mediaType === 'video' && !info.linkUrl && !/^https?:/i.test(src)) {
      const url = (await resolveTarget(tab, info.frameId)) || referer;
      if (url) sendToApp({ url, referer, kind: 'video', tabId: tab?.id });
      return;
    }
    const url = info.linkUrl || src;
    if (url) sendToApp({ url, referer, kind: 'file', tabId: tab?.id });
  } else if (info.menuItemId === 'dm-page-video') {
    const url = (await resolveTarget(tab, info.frameId)) || info.frameUrl || info.pageUrl || tab?.url;
    if (url) sendToApp({ url, referer, kind: 'video', tabId: tab?.id });
  }
});

// ---------- Toolbar icon: jump to the app's live downloads page (pause / stop / progress) ----------
async function openDownloadsTab() {
  try {
    const r = await fetch(base(await getSettings()) + '/ui-url', { headers: BRIDGE_HEADERS });
    const j = await r.json();
    if (!j.ok || !j.url) throw new Error('no url');
    const [tb] = await api.tabs.query({ url: j.url + '*' });
    if (tb) { await api.tabs.update(tb.id, { active: true }); await api.windows.update(tb.windowId, { focused: true }); }
    else await api.tabs.create({ url: j.url });
  } catch {
    notify(t('notifOfflineTitle'), t('notifOfflineBody'));
  }
}
api.action.onClicked.addListener(openDownloadsTab);

// ---------- Popup / content script messages ----------
api.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg?.type) {
        case 'status': {
          const settings = await getSettings();
          const app = await ping(settings);
          sendResponse({ online: !!app, app, settings, clickIntercept: !api.downloads?.onDeterminingFilename, isSafari: IS_SAFARI, captureExtensions: (app && app.captureExtensions) || [] });
          break;
        }
        case 'getMedia': sendResponse(await readMedia(msg.tabId)); break;
        case 'downloads': sendResponse(await getDownloads()); break;
        case 'action': sendResponse(await downloadAction(msg.id, msg.action)); break;
        case 'download': sendResponse(await sendToApp({ ...msg, tabId: sender.tab ? sender.tab.id : undefined })); break;
        case 'reveal': sendResponse(await downloadAction(msg.id, 'reveal')); break;
        case 'setSettings': await setSettings(msg.settings || {}); sendResponse({ ok: true }); break;
        case 'setSiteDisabled': {
          const s = await getSettings();
          const host = String(msg.host || '').toLowerCase().replace(/^www\./, '');
          if (host) {
            const list = new Set(s.disabledSites || []);
            if (msg.disabled) list.add(host); else list.delete(host);
            await setSettings({ disabledSites: [...list].sort() });
            if (msg.disabled && sender.tab == null && msg.tabId != null) await clearMedia(msg.tabId);
          }
          sendResponse({ ok: true });
          break;
        }
        case 'openUI': await openDownloadsTab(); sendResponse({ ok: true }); break;
        case 'launchApp': sendResponse({ ok: await launchApp() }); break;
        case 'pageMedia': {
          if (sender.tab) {
            for (const m of msg.items || []) {
              const kind = classify(m.url, m.type || '');
              if (kind || MEDIA_EXT.test(m.url)) {
                await addMedia(sender.tab.id, { url: m.url, type: m.type || '', size: 0, kind: kind || 'media', time: Date.now(), name: m.name });
              }
            }
          }
          sendResponse({ ok: true });
          break;
        }
        default: sendResponse(null);
      }
    } catch (e) {
      sendResponse({ ok: false, error: String(e) });
    }
  })();
  return true;
});
