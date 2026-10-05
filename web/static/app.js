const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const state = {
  me: null,
  clips: [],
  files: [],
  sessions: [],
  events: null,
  reconnectTimer: null,
  reconnectDelay: 1000,
  tab: 'clips',
};

const store = {
  get(key, fallback = null) {
    try { return localStorage.getItem(key) ?? fallback; } catch { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem(key, value); } catch { /* storage unavailable */ }
  },
};

class HttpError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(path, { method = 'GET', json } = {}) {
  const headers = {};
  if (state.me && method !== 'GET') headers['X-CSRF-Token'] = state.me.csrf_token;
  let body;
  if (json !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(json);
  }
  let res;
  try {
    res = await fetch(path, { method, headers, body, credentials: 'same-origin', cache: 'no-store' });
  } catch {
    throw new HttpError(0, 'Network error: is the server reachable?');
  }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    if (res.status === 401 && path !== '/api/login') signedOut();
    throw new HttpError(res.status, data?.error || `Request failed (${res.status})`);
  }
  return data;
}

/* ---------- formatting ---------- */

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}`;
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
function ago(ms) {
  const s = Math.round((ms - Date.now()) / 1000);
  const abs = Math.abs(s);
  if (abs < 45) return 'just now';
  if (abs < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  if (abs < 86400 * 30) return rtf.format(Math.round(s / 86400), 'day');
  return new Date(ms).toLocaleDateString();
}

function setTime(el, ms) {
  el.dataset.ts = ms;
  el.dateTime = new Date(ms).toISOString();
  el.title = new Date(ms).toLocaleString();
  el.textContent = ago(ms);
}

setInterval(() => $$('time[data-ts]').forEach((t) => { t.textContent = ago(Number(t.dataset.ts)); }), 30000);

function clone(id) {
  return document.getElementById(id).content.firstElementChild.cloneNode(true);
}

/* ---------- toasts ---------- */

function toast(message, kind = 'info') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = message;
  el.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  const box = $('#toasts');
  box.append(el);
  while (box.children.length > 3) box.firstElementChild.remove();
  setTimeout(() => el.classList.add('out'), kind === 'error' ? 5000 : 2500);
  setTimeout(() => el.remove(), kind === 'error' ? 5400 : 2900);
}

/* ---------- clipboard helpers ---------- */

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    // Chromium leaves writeText pending forever while the window is not
    // frontmost, so give up after a moment and use the fallback instead.
    const timeout = new Promise((_, reject) => { setTimeout(() => reject(new Error('timeout')), 1500); });
    try { await Promise.race([navigator.clipboard.writeText(text), timeout]); return true; } catch { /* fall back below */ }
  }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.className = 'offscreen';
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  return ok;
}

/* ---------- views ---------- */

function show(view) {
  $('#boot').hidden = true;
  $('#login-view').hidden = view !== 'login';
  $('#app-view').hidden = view !== 'app';
}

function signedOut() {
  if (!state.me) return;
  state.me = null;
  closeEvents();
  state.clips = [];
  state.files = [];
  show('login');
  toast('You have been signed out.', 'error');
}

function selectTab(tab) {
  state.tab = tab;
  store.set('tab', tab);
  $$('.tabs [data-tab]').forEach((b) => b.setAttribute('aria-selected', String(b.dataset.tab === tab)));
  $$('.tab').forEach((s) => { s.hidden = s.id !== `tab-${tab}`; });
  if (tab === 'account') loadAccount();
}

/* ---------- login ---------- */

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  const err = $('#login-error');
  err.textContent = '';
  const data = Object.fromEntries(new FormData(form));
  if (!data.username || !data.password) {
    err.textContent = 'Enter your username and password.';
    return;
  }
  const btn = $('button[type=submit]', form);
  btn.disabled = true;
  try {
    const me = await api('/api/login', { method: 'POST', json: data });
    store.set('device_name', data.device_name || '');
    form.password.value = '';
    start(me);
  } catch (ex) {
    err.textContent = ex.message;
  } finally {
    btn.disabled = false;
  }
});

/* ---------- clips ---------- */

const CLAMP_CHARS = 500;
const CLAMP_LINES = 8;

function clipNode(clip) {
  const li = clone('clip-tpl');
  li.dataset.id = clip.id;
  const pre = $('.clip-text', li);
  pre.textContent = clip.content;
  const long = clip.content.length > CLAMP_CHARS || clip.content.split('\n').length > CLAMP_LINES;
  if (long) {
    pre.classList.add('clamped');
    const more = $('.more', li);
    more.hidden = false;
    more.addEventListener('click', () => {
      const clamped = pre.classList.toggle('clamped');
      more.textContent = clamped ? 'Show more' : 'Show less';
    });
  }
  $('.device', li).textContent = clip.session_id === state.me.session_id ? 'This device' : clip.device_name;
  setTime($('time', li), clip.created_at);
  $('.copy', li).addEventListener('click', async () => {
    toast(await copyText(clip.content) ? 'Copied to clipboard' : 'Copy failed: select the text and copy manually', 'info');
  });
  $('.delete', li).addEventListener('click', async () => {
    try { await api(`/api/clips/${clip.id}`, { method: 'DELETE' }); removeClip(clip.id); } catch (ex) { toast(ex.message, 'error'); }
  });
  return li;
}

function renderClips() {
  const list = $('#clip-list');
  list.replaceChildren(...state.clips.map(clipNode));
  $('#clip-empty').hidden = state.clips.length > 0;
  $('#clear-clips').hidden = state.clips.length === 0;
}

function addClip(clip) {
  if (state.clips.some((c) => c.id === clip.id)) return false;
  state.clips.unshift(clip);
  state.clips.sort((a, b) => b.id - a.id);
  const limit = state.me.limits.clip_history_limit;
  if (state.clips.length > limit) state.clips.length = limit;
  renderClips();
  return true;
}

function removeClip(id) {
  state.clips = state.clips.filter((c) => c.id !== id);
  renderClips();
}

async function sendClip(text) {
  if (!text) return false;
  const max = state.me.limits.max_clip_bytes;
  if (new TextEncoder().encode(text).length > max) {
    toast(`Text is larger than the ${formatBytes(max)} clip limit. Save it as a file instead.`, 'error');
    return false;
  }
  try {
    const clip = await api('/api/clips', { method: 'POST', json: { content: text } });
    addClip(clip);
    toast('Sent to your devices');
    return true;
  } catch (ex) {
    toast(ex.message, 'error');
    return false;
  }
}

$('#clip-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const input = $('#clip-input');
  if (await sendClip(input.value)) input.value = '';
});

$('#clip-input').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    $('#clip-form').requestSubmit();
  }
});

$('#paste-btn').addEventListener('click', async () => {
  if (navigator.clipboard?.readText) {
    try {
      const text = await navigator.clipboard.readText();
      if (!text) { toast('Your clipboard is empty or holds no text.', 'error'); return; }
      await sendClip(text);
      return;
    } catch { /* permission denied or unsupported: fall through */ }
  }
  $('#clip-input').focus();
  toast('Clipboard access is blocked here. Paste into the box (long-press or Ctrl+V) and press Send.', 'error');
});

$('#clear-clips').addEventListener('click', async () => {
  if (!confirm('Delete your whole clipboard history on all devices?')) return;
  try { await api('/api/clips', { method: 'DELETE' }); state.clips = []; renderClips(); } catch (ex) { toast(ex.message, 'error'); }
});

const autocopy = $('#autocopy');
autocopy.checked = store.get('autocopy') === '1';
autocopy.addEventListener('change', () => store.set('autocopy', autocopy.checked ? '1' : '0'));

async function incomingClip(clip) {
  if (!addClip(clip)) return;
  // Browsers only allow clipboard writes from a focused page.
  if (autocopy.checked && document.hasFocus() && await copyText(clip.content)) {
    toast(`Copied from ${clip.device_name}`);
  } else {
    toast(`New clip from ${clip.device_name}`);
  }
}

/* ---------- files ---------- */

function fileExt(name) {
  const m = /\.([A-Za-z0-9]{1,5})$/.exec(name);
  return m ? m[1].toUpperCase() : 'FILE';
}

function fileNode(f) {
  const li = clone('file-tpl');
  li.dataset.id = f.id;
  const url = `/api/files/${encodeURIComponent(f.id)}`;
  const thumb = $('.thumb', li);
  if (f.previewable) {
    const img = document.createElement('img');
    img.src = `${url}?inline=1`;
    img.alt = '';
    img.loading = 'lazy';
    img.decoding = 'async';
    const link = document.createElement('a');
    link.href = `${url}?inline=1`;
    link.target = '_blank';
    link.rel = 'noopener';
    link.title = 'Open preview';
    link.append(img);
    thumb.replaceChildren(link);
  } else {
    $('.ext', li).textContent = fileExt(f.name);
  }
  $('.file-name', li).textContent = f.name;
  $('.file-name', li).title = f.name;
  $('.size', li).textContent = formatBytes(f.size);
  $('.device', li).textContent = f.session_id === state.me.session_id ? 'This device' : f.device_name;
  setTime($('time', li), f.created_at);
  const dl = $('.download', li);
  dl.href = url;
  dl.download = f.name;
  $('.delete', li).addEventListener('click', async () => {
    if (!confirm(`Delete "${f.name}" from all devices?`)) return;
    try { await api(url, { method: 'DELETE' }); removeFile(f.id); } catch (ex) { toast(ex.message, 'error'); }
  });
  return li;
}

function renderFiles() {
  $('#file-list').replaceChildren(...state.files.map(fileNode));
  $('#file-empty').hidden = state.files.length > 0 || $('#upload-list').children.length > 0;
  renderQuota();
}

function renderQuota() {
  const { limits, used_bytes: used } = state.me;
  $('#limit-hint').textContent = `Up to ${formatBytes(limits.max_upload_bytes)} per file`;
  const quota = limits.user_quota_bytes;
  $('#quota').hidden = !quota;
  if (!quota) return;
  const pct = Math.min(100, (used / quota) * 100);
  $('#quota-fill').style.width = `${pct}%`;
  $('#quota-fill').classList.toggle('high', pct > 90);
  $('#quota-text').textContent = `${formatBytes(used)} of ${formatBytes(quota)} used`;
}

function addFile(f) {
  if (state.files.some((x) => x.id === f.id)) return false;
  state.files.unshift(f);
  state.files.sort((a, b) => b.created_at - a.created_at);
  state.me.used_bytes += f.size;
  renderFiles();
  return true;
}

function removeFile(id) {
  const f = state.files.find((x) => x.id === id);
  if (!f) return;
  state.files = state.files.filter((x) => x.id !== id);
  state.me.used_bytes = Math.max(0, state.me.used_bytes - f.size);
  renderFiles();
}

const uploadQueue = [];
let activeUploads = 0;
const MAX_PARALLEL_UPLOADS = 2;

function queueUploads(fileList) {
  const files = [...fileList];
  if (!files.length) return;
  selectTab('files');
  const max = state.me.limits.max_upload_bytes;
  for (const file of files) {
    if (file.size > max) {
      toast(`"${file.name}" is ${formatBytes(file.size)}, over the ${formatBytes(max)} limit.`, 'error');
      continue;
    }
    const row = clone('upload-tpl');
    $('.file-name', row).textContent = file.name || 'pasted file';
    $('.status', row).textContent = `Waiting · ${formatBytes(file.size)}`;
    $('#upload-list').append(row);
    const job = { file, row, xhr: null, cancelled: false };
    $('.cancel', row).addEventListener('click', () => {
      job.cancelled = true;
      if (job.xhr) job.xhr.abort();
      row.remove();
      renderFiles();
    });
    uploadQueue.push(job);
  }
  renderFiles();
  pumpUploads();
}

function pumpUploads() {
  while (activeUploads < MAX_PARALLEL_UPLOADS && uploadQueue.length) {
    const job = uploadQueue.shift();
    if (job.cancelled) continue;
    activeUploads++;
    upload(job).finally(() => { activeUploads--; pumpUploads(); });
  }
}

function upload(job) {
  const { file, row } = job;
  return new Promise((resolve) => {
    const progress = $('progress', row);
    const status = $('.status', row);
    const xhr = new XMLHttpRequest();
    job.xhr = xhr;
    const started = performance.now();
    const name = file.name || `pasted-${new Date().toISOString().replace(/[:.]/g, '-')}`;
    xhr.open('POST', `/api/files?name=${encodeURIComponent(name)}`);
    xhr.setRequestHeader('X-CSRF-Token', state.me.csrf_token);
    xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream');
    xhr.responseType = 'json';
    xhr.upload.addEventListener('progress', (e) => {
      if (!e.lengthComputable) return;
      progress.value = (e.loaded / e.total) * 100;
      const secs = (performance.now() - started) / 1000;
      const rate = secs > 0.5 ? ` · ${formatBytes(e.loaded / secs)}/s` : '';
      status.textContent = `${formatBytes(e.loaded)} of ${formatBytes(e.total)}${rate}`;
    });
    const done = (msg) => {
      if (msg) {
        row.classList.add('failed');
        progress.hidden = true;
        status.textContent = msg;
        const btn = $('.cancel', row);
        btn.textContent = 'Dismiss';
        toast(`Upload failed: ${msg}`, 'error');
      } else {
        row.remove();
      }
      renderFiles();
      resolve();
    };
    xhr.addEventListener('load', () => {
      if (xhr.status === 201) {
        addFile(xhr.response);
        done();
      } else {
        if (xhr.status === 401) signedOut();
        done(xhr.response?.error || `server responded ${xhr.status}`);
      }
    });
    xhr.addEventListener('error', () => done('network error or the proxy rejected the upload (check its body size limit)'));
    xhr.addEventListener('abort', () => resolve());
    status.textContent = `Starting · ${formatBytes(file.size)}`;
    xhr.send(file);
  });
}

$('#file-input').addEventListener('change', (e) => {
  queueUploads(e.target.files);
  e.target.value = '';
});

const dropzone = $('#dropzone');
let dragDepth = 0;
window.addEventListener('dragenter', (e) => {
  if (!state.me || !e.dataTransfer?.types.includes('Files')) return;
  dragDepth++;
  dropzone.classList.add('active');
});
window.addEventListener('dragleave', () => {
  dragDepth = Math.max(0, dragDepth - 1);
  if (!dragDepth) dropzone.classList.remove('active');
});
window.addEventListener('dragover', (e) => e.preventDefault());
window.addEventListener('drop', (e) => {
  e.preventDefault();
  dragDepth = 0;
  dropzone.classList.remove('active');
  if (state.me && e.dataTransfer?.files.length) queueUploads(e.dataTransfer.files);
});

document.addEventListener('paste', (e) => {
  if (!state.me) return;
  const t = e.target;
  if (t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement || t.isContentEditable) return;
  const files = e.clipboardData?.files;
  if (files && files.length) {
    e.preventDefault();
    queueUploads(files);
    return;
  }
  const text = e.clipboardData?.getData('text/plain');
  if (text) {
    e.preventDefault();
    selectTab('clips');
    sendClip(text);
  }
});

/* ---------- account ---------- */

async function loadAccount() {
  if (!state.me) return;
  $('#who').textContent = state.me.user.username;
  const nameInput = $('#device-name');
  if (document.activeElement !== nameInput) nameInput.value = state.me.device_name;
  try {
    state.sessions = await api('/api/sessions');
    renderSessions();
  } catch (ex) { toast(ex.message, 'error'); }
  $('#admin-card').hidden = !state.me.user.is_admin;
  if (state.me.user.is_admin) loadUsers();
}

function renderSessions() {
  $('#session-list').replaceChildren(...state.sessions.map((s) => {
    const li = clone('session-tpl');
    $('.name', li).textContent = s.device_name;
    $('.dot', li).classList.toggle('online', s.online);
    $('.dot', li).title = s.online ? 'Connected now' : 'Not connected';
    $('.current', li).hidden = !s.current;
    $('.ua', li).textContent = s.user_agent || 'unknown browser';
    $('.ip', li).textContent = s.ip;
    setTime($('time', li), s.last_seen);
    const revoke = $('.revoke', li);
    revoke.addEventListener('click', async () => {
      if (s.current) { logout(); return; }
      if (!confirm(`Sign out "${s.device_name}"?`)) return;
      try { await api(`/api/sessions/${s.id}`, { method: 'DELETE' }); loadAccount(); } catch (ex) { toast(ex.message, 'error'); }
    });
    return li;
  }));
}

async function loadUsers() {
  try {
    const users = await api('/api/admin/users');
    $('#user-list').replaceChildren(...users.map((u) => {
      const li = clone('user-tpl');
      const self = u.id === state.me.user.id;
      $('.name', li).textContent = u.username + (self ? ' (you)' : '');
      $('.admin', li).hidden = !u.is_admin;
      $('.used', li).textContent = formatBytes(u.used_bytes);
      const toggle = $('.toggle-admin', li);
      toggle.textContent = u.is_admin ? 'Remove admin' : 'Make admin';
      for (const b of $$('.actions button', li)) b.hidden = self;
      $('.reset', li).addEventListener('click', async () => {
        const pw = prompt(`New password for ${u.username} (min 8 characters). They will be signed out everywhere.`);
        if (!pw) return;
        try { await api(`/api/admin/users/${u.id}`, { method: 'PATCH', json: { password: pw } }); toast('Password reset'); } catch (ex) { toast(ex.message, 'error'); }
      });
      toggle.addEventListener('click', async () => {
        try { await api(`/api/admin/users/${u.id}`, { method: 'PATCH', json: { is_admin: !u.is_admin } }); loadUsers(); } catch (ex) { toast(ex.message, 'error'); }
      });
      $('.delete', li).addEventListener('click', async () => {
        if (!confirm(`Delete ${u.username} and all of their clips and files? This cannot be undone.`)) return;
        try { await api(`/api/admin/users/${u.id}`, { method: 'DELETE' }); loadUsers(); } catch (ex) { toast(ex.message, 'error'); }
      });
      return li;
    }));
  } catch (ex) { toast(ex.message, 'error'); }
}

$('#device-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const res = await api(`/api/sessions/${state.me.session_id}`, { method: 'PATCH', json: { device_name: $('#device-name').value } });
    state.me.device_name = res.device_name;
    store.set('device_name', res.device_name);
    toast('Device renamed');
    loadAccount();
  } catch (ex) { toast(ex.message, 'error'); }
});

$('#password-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  try {
    const res = await api('/api/password', {
      method: 'POST',
      json: { current_password: form.current_password.value, new_password: form.new_password.value },
    });
    form.reset();
    toast(`Password updated. Signed out ${res.revoked_sessions} other device(s).`);
    loadAccount();
  } catch (ex) { toast(ex.message, 'error'); }
});

$('#user-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  try {
    await api('/api/admin/users', {
      method: 'POST',
      json: { username: form.username.value.trim(), password: form.password.value, is_admin: form.is_admin.checked },
    });
    form.reset();
    toast('User created');
    loadUsers();
  } catch (ex) { toast(ex.message, 'error'); }
});

async function logout() {
  try { await api('/api/logout', { method: 'POST' }); } catch { /* signing out anyway */ }
  state.me = null;
  closeEvents();
  show('login');
}

$('#logout-btn').addEventListener('click', logout);

/* ---------- real-time events ---------- */

function setConn(s) {
  const el = $('#conn');
  el.dataset.state = s;
  el.textContent = { live: 'Live', connecting: 'Connecting', offline: 'Offline' }[s];
}

function closeEvents() {
  clearTimeout(state.reconnectTimer);
  if (state.events) state.events.close();
  state.events = null;
}

function connectEvents() {
  closeEvents();
  setConn('connecting');
  const es = new EventSource('/api/events');
  state.events = es;
  es.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    handleEvent(ev);
  };
  es.onerror = () => {
    if (state.events !== es) return;
    setConn(es.readyState === EventSource.CLOSED ? 'offline' : 'connecting');
    // EventSource retries transient drops itself but gives up for good on an
    // HTTP error such as 401, so that case needs a manual check and retry.
    if (es.readyState === EventSource.CLOSED) scheduleReconnect();
  };
}

function scheduleReconnect() {
  clearTimeout(state.reconnectTimer);
  state.reconnectTimer = setTimeout(async () => {
    try {
      state.me = { ...state.me, ...(await api('/api/me')) };
      state.reconnectDelay = 1000;
      connectEvents();
    } catch (ex) {
      if (ex.status === 401) return;
      state.reconnectDelay = Math.min(state.reconnectDelay * 2, 30000);
      scheduleReconnect();
    }
  }, state.reconnectDelay);
}

function handleEvent(ev) {
  const mine = ev.from === state.me?.session_id;
  switch (ev.type) {
    case 'hello':
      setConn('live');
      state.reconnectDelay = 1000;
      refresh();
      break;
    case 'clip':
      if (mine) addClip(ev.data); else incomingClip(ev.data);
      break;
    case 'clip_deleted':
      removeClip(ev.data.id);
      break;
    case 'clips_cleared':
      state.clips = [];
      renderClips();
      break;
    case 'file':
      if (addFile(ev.data) && !mine) toast(`New file from ${ev.data.device_name}: ${ev.data.name}`);
      break;
    case 'file_deleted':
      removeFile(ev.data.id);
      break;
    case 'devices_changed':
      if (state.tab === 'account') loadAccount();
      break;
    case 'resync':
      refresh();
      break;
  }
}

async function refresh() {
  if (!state.me) return;
  try {
    const [me, clips, files] = await Promise.all([api('/api/me'), api('/api/clips'), api('/api/files')]);
    state.me = me;
    state.clips = clips;
    state.files = files;
    renderClips();
    renderFiles();
    if (state.tab === 'account') loadAccount();
  } catch (ex) {
    if (ex.status !== 401) toast(ex.message, 'error');
  }
}

$('#refresh-btn').addEventListener('click', async () => {
  await refresh();
  if (!state.events || state.events.readyState === EventSource.CLOSED) connectEvents();
});

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'visible' || !state.me) return;
  // Mobile browsers often kill background connections without an error event.
  if (!state.events || state.events.readyState !== EventSource.OPEN) connectEvents();
  else refresh();
});

/* ---------- boot ---------- */

$$('.tabs [data-tab]').forEach((b) => b.addEventListener('click', () => selectTab(b.dataset.tab)));

function start(me) {
  state.me = me;
  state.reconnectDelay = 1000;
  show('app');
  renderClips();
  renderFiles();
  selectTab(['clips', 'files', 'account'].includes(store.get('tab')) ? store.get('tab') : 'clips');
  connectEvents();
}

(async () => {
  const deviceInput = $('#login-form').device_name;
  deviceInput.value = store.get('device_name', '');
  try {
    start(await api('/api/me'));
  } catch {
    show('login');
    $('#login-form').username.focus();
  }
})();
