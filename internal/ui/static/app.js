'use strict';

const $ = (id) => document.getElementById(id);

const state = {
  clips: [],
  saved: [],        // JSON of each clip as last loaded/saved, to mark edits
  version: '',
  status: {},
  editing: -1,      // index being edited, -1 for new
  probe: null,      // {path, duration, keyframes, byte_range, note, resolved}
  probeError: '',
  dirty: false,
};

// ---------- helpers ----------

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === 'class') node.className = v;
    else if (k === 'on') for (const [ev, fn] of Object.entries(v)) node.addEventListener(ev, fn);
    else node[k] = v;
  }
  for (const c of children) if (c != null) node.append(c);
  return node;
}

function parseTime(text) {
  const s = String(text).trim();
  if (s === '') return NaN;
  if (/^\d+(\.\d+)?$/.test(s)) return parseFloat(s);
  const m = /^(?:(\d+):)?(\d{1,2}):(\d{1,2}(?:\.\d+)?)$/.exec(s);
  if (!m) return NaN;
  return (parseInt(m[1] || '0', 10) * 3600) + (parseInt(m[2], 10) * 60) + parseFloat(m[3]);
}

function formatTime(sec) {
  if (!Number.isFinite(sec)) return '';
  const neg = sec < 0;
  const ms = Math.round(Math.abs(sec) * 1000);
  const h = Math.floor(ms / 3600000);
  const m = Math.floor((ms % 3600000) / 60000);
  const s = (ms % 60000) / 1000;
  const ss = (s < 10 ? '0' : '') + s.toFixed(3).replace(/\.?0+$/, '');
  return (neg ? '-' : '') + (h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`);
}

function formatBytes(n) {
  if (!n) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

function clipPath(c) {
  const g = (c.group || '').replace(/^\/+|\/+$/g, '');
  return g ? `${g}/${c.output}` : c.output;
}

let toastTimer;
function toast(msg, kind = 'success') {
  const t = $('toast');
  t.textContent = msg;
  t.className = kind;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, kind === 'error' ? 8000 : 3500);
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: opts.body ? { 'Content-Type': 'application/json' } : {},
    credentials: 'same-origin',
  });
  let data = null;
  if ((res.headers.get('Content-Type') || '').includes('application/json')) data = await res.json();
  if (!res.ok) {
    const err = new Error((data && data.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function setDirty(d) {
  state.dirty = d;
  $('btn-save').disabled = !d;
  $('dirty').hidden = !d;
}

// ---------- clip list ----------

async function load() {
  try {
    const data = await api('api/clips');
    state.clips = data.clips;
    state.saved = data.clips.map((c) => JSON.stringify(c));
    state.version = data.version;
    state.status = data.status || {};
    const sel = $('f-nfotype');
    while (sel.options.length > 1) sel.remove(1);
    for (const t of data.nfo_types) sel.append(el('option', { value: t, textContent: t }));
    setDirty(false);
    renderBackup(data.backup);
    render();
  } catch (e) {
    toast(`Couldn't load clips: ${e.message}`, 'error');
  }
}

async function refreshStatus() {
  if (document.hidden) return;
  try {
    const data = await api('api/clips');
    state.status = data.status || {};
    renderBackup(data.backup);
    if (!state.dirty && data.version !== state.version) return load();
    render();
  } catch { /* transient; next poll retries */ }
}

function ago(date) {
  const s = Math.max(0, (Date.now() - date.getTime()) / 1000);
  if (s < 90) return 'just now';
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} days ago`;
}

function renderBackup(b) {
  const node = $('backup');
  node.hidden = !b;
  if (!b) return;
  node.classList.toggle('warn', !!b.error);
  const latest = b.latest && !b.latest.startsWith('0001') ? new Date(b.latest) : null;
  node.textContent = b.error ? `Backup failed: ${b.error}`
    : latest ? `Backed up ${ago(latest)} (${b.copies} kept)` : 'No backup yet';
  node.title = `Backups in ${b.dir}`;
}

function statusBadge(c, i) {
  if (state.saved[i] !== JSON.stringify(c)) return el('span', { class: 'badge unsaved', textContent: 'unsaved' });
  const st = state.status[clipPath(c)];
  if (!st) return el('span', { class: 'badge pending', textContent: 'pending' });
  return el('span', { class: `badge ${st.state}`, textContent: st.state, title: st.error || (st.kind === 'remux' ? 'Copied into the cache (non-Matroska source)' : 'Served by reference from the source file') });
}

function render() {
  const body = $('clips');
  body.replaceChildren();
  const q = $('filter').value.trim().toLowerCase();
  const rows = state.clips
    .map((c, i) => ({ c, i }))
    .filter(({ c }) => !q || `${clipPath(c)} ${c.input}`.toLowerCase().includes(q))
    .sort((a, b) => clipPath(a.c).localeCompare(clipPath(b.c)));
  for (const { c, i } of rows) {
    const st = state.status[clipPath(c)];
    const actual = st && st.state === 'ready' && st.actual_start != null && Math.abs(st.actual_start - c.start) > 0.01
      ? el('div', { class: 'group', textContent: `starts at ${formatTime(st.actual_start)} (keyframe)` }) : null;
    const upgraded = st && st.source && st.source !== c.input.replace(/^\/+/, '') && !c.input.endsWith(`/${st.source}`)
      ? el('div', { class: 'group warn', textContent: `file replaced; using ${st.source.split('/').pop()}` }) : null;
    const tr = el('tr', { class: state.saved[i] !== JSON.stringify(c) ? 'changed' : '' },
      el('td', {}, el('div', { textContent: c.output }), c.group ? el('div', { class: 'group', textContent: c.group }) : null),
      el('td', { class: 'source' }, c.input, upgraded),
      el('td', {}, formatTime(c.start), actual),
      el('td', { textContent: formatTime(c.end - c.start) }),
      el('td', {}, statusBadge(c, i), st && st.size ? el('div', { class: 'group', textContent: formatBytes(st.size) }) : null,
        st && st.error ? el('div', { class: 'error', textContent: st.error }) : null),
      el('td', {}, el('button', { textContent: 'Edit', on: { click: () => openEditor(i) } })));
    body.append(tr);
  }
  $('empty').hidden = state.clips.length > 0;
}

// ---------- editor ----------

function openEditor(i) {
  state.editing = i;
  const c = i >= 0 ? state.clips[i] : { input: '', output: '', start: 0, end: 0 };
  $('editor-title').textContent = i >= 0 ? 'Edit clip' : 'New clip';
  $('f-input').value = c.input || '';
  $('f-start').value = i >= 0 ? formatTime(c.start) : '';
  $('f-end').value = i >= 0 ? formatTime(c.end) : '';
  $('f-output').value = c.output || '';
  $('f-group').value = c.group || '';
  $('f-nfotype').value = c.nfo_type || '';
  $('f-metadata').value = c.metadata ? JSON.stringify(c.metadata, null, 2) : '';
  $('editor-error').textContent = '';
  $('btn-delete').hidden = i < 0;
  $('preview').hidden = true;
  $('preview').removeAttribute('src');
  state.probe = null;
  state.probeError = '';
  $('probe-info').textContent = '';
  updateTimes();
  $('editor').showModal();
  if (c.input) probe();
}

async function probe() {
  const path = $('f-input').value.trim();
  state.probe = null;
  state.probeError = '';
  $('probe-info').textContent = path ? 'Reading source…' : '';
  updateTimes();
  if (!path) return;
  try {
    const p = await api(`api/probe?path=${encodeURIComponent(path)}`);
    if ($('f-input').value.trim() !== path) return;
    state.probe = { path, ...p };
    const kf = p.keyframes.length;
    $('probe-info').textContent = (p.resolved ? `This file was replaced; clips use ${p.resolved}. ` : '')
      + `Length ${formatTime(p.duration)}. ` + (p.byte_range
      ? `Clips are served straight from this file (no extra disk space); ${kf} keyframes.`
      : (p.note || ''));
  } catch (e) {
    if ($('f-input').value.trim() !== path) return;
    state.probeError = e.message;
    $('probe-info').textContent = e.message;
  }
  updateTimes();
}

// keyframeAtOrBefore returns the time a byte-range clip starting at t really starts.
function keyframeAtOrBefore(t) {
  const kf = state.probe && state.probe.keyframes;
  if (!kf || !kf.length) return t;
  let lo = 0, hi = kf.length - 1, best = kf[0];
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (kf[mid] <= t + 1e-6) { best = kf[mid]; lo = mid + 1; } else hi = mid - 1;
  }
  return best;
}

let frameTimer;
function updateTimes() {
  const start = parseTime($('f-start').value);
  const end = parseTime($('f-end').value);
  const p = state.probe;
  let actual = start;
  if (Number.isFinite(start) && p && p.byte_range) {
    actual = keyframeAtOrBefore(start);
    const early = start - actual;
    $('start-info').textContent = early > 0.05
      ? `Plays from ${formatTime(actual)}: the keyframe ${early.toFixed(1)} s before.`
      : `Starts exactly on a keyframe.`;
  } else {
    $('start-info').textContent = Number.isFinite(start) ? '' : 'Use h:mm:ss, m:ss or seconds.';
  }
  if (Number.isFinite(start) && Number.isFinite(end)) {
    $('end-info').textContent = end > start ? `Length ${formatTime(end - actual)}` : 'End must be after the start.';
    if (p && p.duration && end > p.duration) $('end-info').textContent = `The source is only ${formatTime(p.duration)} long.`;
  } else {
    $('end-info').textContent = '';
  }
  $('btn-prev-key').disabled = $('btn-next-key').disabled = !(p && p.keyframes.length);
  const dur = p && p.duration ? p.duration : Infinity;
  let startWhy = '', endWhy = '';
  if (!p) startWhy = endWhy = state.probeError ? "Can't read this source"
    : $('f-input').value.trim() ? 'Reading the source…' : 'Choose a source video';
  else if (!Number.isFinite(start)) startWhy = 'Enter a start time';
  else if (start >= dur) startWhy = 'The start is past the end of the video';
  if (p && !endWhy) {
    if (!Number.isFinite(end)) endWhy = 'Enter an end time';
    else if (Number.isFinite(start) && end <= start) endWhy = 'The end must be after the start';
  }
  clearTimeout(frameTimer);
  frameTimer = setTimeout(() => {
    setFrame('start-frame', startWhy ? NaN : actual, startWhy);
    // The last frame before the end; near the end of the file the video can stop before the audio.
    const endAt = end > dur - 1 ? dur - 1 : end - 0.1;
    setFrame('end-frame', endWhy ? NaN : Math.max(Number.isFinite(actual) ? actual : 0, endAt), endWhy);
  }, 400);
}

function setFrame(id, t, why) {
  const img = $(id);
  const msg = $(`${id}-msg`);
  if (!Number.isFinite(t) || !state.probe) {
    img.hidden = true;
    img.removeAttribute('src');
    msg.textContent = why || '';
    return;
  }
  const url = `api/frame?path=${encodeURIComponent(state.probe.path)}&t=${t.toFixed(3)}`;
  if (img.getAttribute('src') === url) return;
  img.hidden = true;
  msg.textContent = 'Loading frame…';
  img.onload = () => { img.hidden = false; };
  img.onerror = () => { img.hidden = true; msg.textContent = 'No frame at this time'; };
  img.src = url;
}

function stepKeyframe(dir) {
  const kf = state.probe && state.probe.keyframes;
  if (!kf || !kf.length) return;
  const cur = parseTime($('f-start').value);
  const base = Number.isFinite(cur) ? keyframeAtOrBefore(cur) : 0;
  const i = kf.indexOf(base);
  const next = kf[Math.min(kf.length - 1, Math.max(0, i + dir))];
  $('f-start').value = formatTime(next);
  updateTimes();
}

function preview(which) {
  const p = state.probe;
  const start = parseTime($('f-start').value);
  const end = parseTime($('f-end').value);
  if (!p || !Number.isFinite(start) || !Number.isFinite(end) || end <= start) {
    $('editor-error').textContent = 'Choose a source, start and end first.';
    return;
  }
  const from = which === 'start' ? (p.byte_range ? keyframeAtOrBefore(start) : start) : Math.max(start, end - 10);
  const dur = Math.max(1, Math.min(10, end - from));
  const v = $('preview');
  v.src = `api/preview?path=${encodeURIComponent(p.path)}&start=${from.toFixed(3)}&dur=${dur.toFixed(3)}`;
  v.hidden = false;
  v.play().catch(() => {});
  toast('Rendering preview…');
}

function readEditor() {
  const clip = {
    input: $('f-input').value.trim(),
    output: $('f-output').value.trim(),
    start: parseTime($('f-start').value),
    end: parseTime($('f-end').value),
  };
  const errors = [];
  if (!clip.input) errors.push('Choose a source video.');
  if (!clip.output) errors.push('Enter a name.');
  if (!Number.isFinite(clip.start)) errors.push('Start is not a valid time.');
  if (!Number.isFinite(clip.end)) errors.push('End is not a valid time.');
  if (Number.isFinite(clip.start) && Number.isFinite(clip.end) && clip.end <= clip.start) errors.push('End must be after the start.');
  const group = $('f-group').value.trim().replace(/^\/+|\/+$/g, '');
  if (group) clip.group = group;
  const nfo = $('f-nfotype').value;
  if (nfo) clip.nfo_type = nfo;
  const meta = $('f-metadata').value.trim();
  if (meta) {
    try {
      const m = JSON.parse(meta);
      if (m === null || typeof m !== 'object' || Array.isArray(m)) throw new Error('not an object');
      clip.metadata = m;
    } catch (e) {
      errors.push(`Metadata must be a JSON object (${e.message}).`);
    }
  }
  return { clip, errors };
}

function applyEditor(ev) {
  ev.preventDefault();
  const { clip, errors } = readEditor();
  const path = clipPath(clip).toLowerCase();
  if (state.clips.some((c, i) => i !== state.editing && clipPath(c).toLowerCase() === path)) {
    errors.push(`Another clip is already named "${clipPath(clip)}".`);
  }
  if (errors.length) { $('editor-error').textContent = errors.join('\n'); return; }
  if (state.editing >= 0) state.clips[state.editing] = clip;
  else { state.clips.push(clip); state.saved.push(null); }
  $('editor').close();
  setDirty(true);
  render();
}

function deleteClip() {
  const i = state.editing;
  if (i < 0 || !confirm(`Delete "${clipPath(state.clips[i])}"?`)) return;
  state.clips.splice(i, 1);
  state.saved.splice(i, 1);
  $('editor').close();
  setDirty(true);
  render();
}

async function save() {
  $('btn-save').disabled = true;
  try {
    const res = await api('api/clips', { method: 'PUT', body: JSON.stringify({ clips: state.clips, version: state.version }) });
    state.version = res.version;
    toast('Saved. Clips appear in the library once they are ready.');
    await load();
  } catch (e) {
    setDirty(true);
    if (e.status === 422 && e.data && e.data.problems) {
      const lines = e.data.problems.map((p) => `${clipPath(state.clips[p.index]) || `#${p.index + 1}`}: ${p.message}`);
      toast(`Not saved:\n${lines.join('\n')}`, 'error');
    } else {
      toast(`Not saved: ${e.message}`, 'error');
    }
  }
}

// ---------- media browser ----------

async function browse(path) {
  try {
    const data = await api(`api/browse?path=${encodeURIComponent(path)}`);
    $('browse-path').textContent = `/${data.path}`;
    const list = $('browse-list');
    list.replaceChildren();
    if (data.path) {
      const up = data.path.split('/').slice(0, -1).join('/');
      list.append(el('li', { class: 'dir', textContent: '..', on: { click: () => browse(up) } }));
    }
    for (const e of data.entries) {
      const rel = data.path ? `${data.path}/${e.name}` : e.name;
      if (e.dir) {
        list.append(el('li', { class: 'dir', textContent: e.name, on: { click: () => browse(rel) } }));
      } else {
        list.append(el('li', { on: { click: () => pick(rel) } },
          el('span', { textContent: e.name }), el('span', { class: 'size', textContent: formatBytes(e.size) })));
      }
    }
    if (!$('browser').open) $('browser').showModal();
  } catch (e) {
    toast(`Couldn't open folder: ${e.message}`, 'error');
  }
}

function pick(rel) {
  $('f-input').value = rel;
  $('browser').close();
  if (!$('f-output').value) $('f-output').value = rel.split('/').pop().replace(/\.[^.]+$/, '');
  probe();
}

// ---------- wiring ----------

$('btn-new').addEventListener('click', () => openEditor(-1));
$('btn-save').addEventListener('click', save);
$('btn-reload').addEventListener('click', () => { if (!state.dirty || confirm('Discard unsaved changes?')) load(); });
$('filter').addEventListener('input', render);
$('editor-form').addEventListener('submit', applyEditor);
$('btn-cancel').addEventListener('click', () => $('editor').close());
$('btn-delete').addEventListener('click', deleteClip);
$('btn-browse').addEventListener('click', () => {
  const cur = $('f-input').value.trim();
  browse(cur.includes('/') ? cur.split('/').slice(0, -1).join('/') : '');
});
$('btn-browse-close').addEventListener('click', () => $('browser').close());
$('f-input').addEventListener('change', probe);
$('f-start').addEventListener('input', updateTimes);
$('f-end').addEventListener('input', updateTimes);
$('btn-prev-key').addEventListener('click', () => stepKeyframe(-1));
$('btn-next-key').addEventListener('click', () => stepKeyframe(1));
$('btn-preview-start').addEventListener('click', () => preview('start'));
$('btn-preview-end').addEventListener('click', () => preview('end'));
window.addEventListener('beforeunload', (ev) => { if (state.dirty) ev.preventDefault(); });
setInterval(refreshStatus, 5000);
load();
