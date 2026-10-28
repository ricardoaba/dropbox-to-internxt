import './style.css';
import * as App from '../wailsjs/go/main/App';
import { main } from '../wailsjs/go/models';
import { EventsOn } from '../wailsjs/runtime/runtime';

// ---------- utilidades DOM ----------

const $ = <T extends HTMLElement>(id: string): T => {
  const el = document.getElementById(id);
  if (!el) throw new Error(`no existe #${id}`);
  return el as T;
};
const input = (id: string) => $<HTMLInputElement>(id);

function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c] as string));
}

function errText(e: unknown): string {
  if (typeof e === 'string') return e;
  if (e instanceof Error) return e.message;
  return String(e);
}

// ---------- registro ----------

const logList = $<HTMLUListElement>('log');
function addLog(time: string, msg: string) {
  const li = document.createElement('li');
  li.innerHTML = `<span class="t">${escapeHtml(time)}</span><span>${escapeHtml(msg)}</span>`;
  logList.appendChild(li);
  while (logList.children.length > 500) logList.removeChild(logList.firstChild!);
  li.scrollIntoView({ block: 'nearest' });
}
function logLocal(msg: string) {
  addLog(new Date().toLocaleTimeString('es-ES'), msg);
}

// ---------- configuración ----------

function cfgToForm(c: main.Config) {
  input('cfg-source').value = c.sourceDir;
  input('cfg-remote').value = c.remoteRoot;
  input('cfg-url').value = c.webdavUrl;
  input('cfg-user').value = c.webdavUser;
  input('cfg-pass').value = c.webdavPass;
  input('cfg-insecure').checked = c.insecure;
  input('cfg-batch-files').value = String(c.batchFiles);
  input('cfg-batch-mb').value = String(c.batchMB);
  $<HTMLSelectElement>('cfg-verify').value = c.verify;
  input('cfg-parallel').value = String(c.parallel);
  input('cfg-retries').value = String(c.retries);
  input('cfg-quarantine').value = String(c.quarantineAfter ?? 3);
  input('cfg-maxgb').value = String(c.maxFileGB);
  $<HTMLTextAreaElement>('cfg-excludes').value = (c.excludes || []).join('\n');
  input('cfg-schedule').value = String(c.scheduleMinutes);
  input('cfg-compose').value = c.composeDir;
  input('cfg-inxt-user').value = c.inxtUser || '';
  input('cfg-inxt-pass').value = c.inxtPassword || '';
  input('cfg-inxt-otp').value = c.inxtOtpToken || '';
  input('cfg-dbx-key').value = c.dropboxAppKey || '';
  input('cfg-fetch-online').checked = !!c.fetchOnlineOnly;
  renderDropboxState(c);
}

function renderDropboxState(c: main.Config) {
  const connected = !!c.dropboxRefreshToken;
  const status = $('dbx-status');
  status.classList.toggle('warn', connected && !!c.dropboxAuthError);
  status.textContent = !connected
    ? 'sin conectar'
    : c.dropboxAuthError
      ? `autorización revocada${c.dropboxAccount ? ' (' + c.dropboxAccount + ')' : ''}: vuelve a conectar`
      : `conectado${c.dropboxAccount ? ' como ' + c.dropboxAccount : ''}`;
  $<HTMLButtonElement>('btn-dbx-disconnect').disabled = !connected;
  input('cfg-fetch-online').disabled = !connected;
}

function formToCfg(base: main.Config): main.Config {
  const c = new main.Config(base);
  c.sourceDir = input('cfg-source').value.trim();
  c.remoteRoot = input('cfg-remote').value.trim() || '/Dropbox';
  c.webdavUrl = input('cfg-url').value.trim();
  c.webdavUser = input('cfg-user').value.trim();
  c.webdavPass = input('cfg-pass').value;
  c.insecure = input('cfg-insecure').checked;
  c.batchFiles = Number(input('cfg-batch-files').value) || 0;
  c.batchMB = Number(input('cfg-batch-mb').value) || 0;
  c.verify = $<HTMLSelectElement>('cfg-verify').value;
  c.parallel = Number(input('cfg-parallel').value) || 1;
  c.retries = Number(input('cfg-retries').value) || 0;
  c.quarantineAfter = Number(input('cfg-quarantine').value) || 0;
  c.maxFileGB = Number(input('cfg-maxgb').value) || 0;
  c.excludes = $<HTMLTextAreaElement>('cfg-excludes').value.split('\n').map(s => s.trim()).filter(Boolean);
  c.scheduleMinutes = Number(input('cfg-schedule').value) || 0;
  c.composeDir = input('cfg-compose').value.trim();
  c.inxtUser = input('cfg-inxt-user').value.trim();
  c.inxtPassword = input('cfg-inxt-pass').value;
  c.inxtOtpToken = input('cfg-inxt-otp').value.trim();
  c.dropboxAppKey = input('cfg-dbx-key').value.trim();
  c.fetchOnlineOnly = input('cfg-fetch-online').checked && !!c.dropboxRefreshToken;
  return c;
}

// ---------- barra de estado ----------

function notify(msg: string, kind: '' | 'ok' | 'err' | 'warn' = '') {
  const bar = $('statusbar');
  bar.textContent = msg;
  bar.className = `statusbar ${kind}`;
  bar.classList.toggle('hidden', msg === '');
}

let currentCfg: main.Config;

function setCfgMsg(text: string, kind: '' | 'ok' | 'err' = '') {
  const el = $('cfg-msg');
  el.textContent = text;
  el.className = `msg ${kind}`;
  if (text) setTimeout(() => { if (el.textContent === text) { el.textContent = ''; el.className = 'msg'; } }, 4000);
}

async function saveConfig() {
  try {
    const c = formToCfg(currentCfg);
    await App.SaveConfig(c);
    currentCfg = await App.GetConfig();
    setCfgMsg('Guardado', 'ok');
    resetBatchTable();
    void testConnection();
  } catch (e) {
    setCfgMsg(errText(e), 'err');
  }
}

// ---------- estado ----------

// Contadores en vivo: se parte del último escaneo y se actualizan con cada
// fichero terminado, sin esperar al reescaneo del final del lote.
const live = { total: 0, totalBytes: 0, done: 0, doneBytes: 0, pending: 0, pendingBytes: 0, skipped: 0, skippedBytes: 0, quarantined: 0, quarantinedBytes: 0 };

// ---------- gráfico de tarta ----------

let pieMode: 'files' | 'batch' = 'files';

// Estado del lote en curso, leído de la tabla: una fila por fichero con su etiqueta.
function batchCounts() {
  const c = { done: 0, failed: 0, skipped: 0, active: 0, deferred: 0, waiting: 0, total: 0 };
  for (const tr of rowByPath.values()) {
    c.total++;
    switch (tr.children[0].textContent) {
      case 'OK': c.done++; break;
      case 'FALLO': c.failed++; break;
      case 'omitido': c.skipped++; break;
      case 'en curso': c.active++; break;
      case 'aplazado': c.deferred++; break;
      default: c.waiting++;
    }
  }
  return c;
}

function renderPie() {
  const byBatch = pieMode === 'batch';
  const b = batchCounts();
  const parts = byBatch
    ? [
      { label: 'Correctos', value: b.done, color: 'var(--ok)', always: true },
      { label: 'Fallidos', value: b.failed, color: '#ff453a', always: false },
      { label: 'Omitidos', value: b.skipped, color: '#8e8e93', always: false },
      { label: 'En curso', value: b.active, color: 'var(--accent)', always: true },
      { label: 'Aplazados', value: b.deferred, color: 'var(--warn)', always: false },
      { label: 'En espera', value: b.waiting, color: '#636366', always: true },
    ]
    : [
      { label: 'Migrados', value: live.done, color: 'var(--ok)', always: true },
      { label: 'Pendientes', value: live.pending, color: 'var(--warn)', always: true },
      { label: 'Aparcados', value: live.quarantined, color: '#ff453a', always: false },
      { label: 'Excluidos', value: live.skipped, color: '#8e8e93', always: false },
    ];
  const total = parts.reduce((a, p) => a + p.value, 0);
  const r = 40, c = 2 * Math.PI * r;
  const g = $('pie-segments');
  g.textContent = '';
  let offset = 0;
  for (const p of parts) {
    if (p.value <= 0 || total <= 0) continue;
    const len = p.value / total * c;
    const seg = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
    seg.setAttribute('cx', '50'); seg.setAttribute('cy', '50'); seg.setAttribute('r', String(r));
    seg.setAttribute('fill', 'none'); seg.setAttribute('stroke', p.color); seg.setAttribute('stroke-width', '16');
    seg.setAttribute('stroke-dasharray', `${len} ${c - len}`);
    seg.setAttribute('stroke-dashoffset', String(-offset));
    seg.setAttribute('transform', 'rotate(-90 50 50)');
    const title = document.createElementNS('http://www.w3.org/2000/svg', 'title');
    title.textContent = `${p.label}: ${p.value} (${(p.value / total * 100).toFixed(1)}%)`;
    seg.appendChild(title);
    g.appendChild(seg);
    offset += len;
  }
  // En el lote cuenta como "terminado" todo lo que ya no va a moverse: correctos, fallidos y omitidos.
  const finished = byBatch ? b.done + b.failed + b.skipped : live.done;
  const donePct = total > 0 ? finished / total * 100 : 0;
  $('pie-center').textContent = total > 0 ? `${donePct.toFixed(donePct >= 99.95 ? 0 : 1)}%` : '–';
  $('pie-center-sub').textContent = total > 0 ? (byBatch ? 'del lote' : 'de los ficheros') : '';
  const legend = $('pie-legend');
  legend.textContent = '';
  if (byBatch && total === 0) {
    const li = document.createElement('li');
    li.textContent = 'Sin lote en curso.';
    legend.appendChild(li);
  }
  for (const p of parts) {
    if (p.value <= 0 && !(p.always && total > 0)) continue;
    const li = document.createElement('li');
    const dot = document.createElement('span');
    dot.className = 'pie-dot'; dot.style.background = p.color;
    li.appendChild(dot);
    const pct = total > 0 ? ` · ${(p.value / total * 100).toFixed(1)}%` : '';
    li.appendChild(document.createTextNode(`${p.label}: ${p.value}${pct}`));
    legend.appendChild(li);
  }
  // Pista para parar o reiniciar sin cortar un lote a medias.
  const hint = $('pie-hint');
  const remaining = total - finished;
  hint.classList.toggle('hidden', !byBatch || total === 0);
  hint.classList.toggle('ready', byBatch && total > 0 && remaining === 0);
  if (byBatch && total > 0) {
    hint.textContent = remaining > 0
      ? `Quedan ${remaining} de ${total}. ${continuous ? 'Para cerrar o reiniciar sin perder trabajo, usa «Parar al acabar el lote».' : 'Para cerrar o reiniciar sin perder trabajo, espera a que acabe.'}`
      : 'Lote terminado: puedes cerrar la app o reiniciar sin perder nada.';
  }
  $('pie-files').classList.toggle('pie-on', !byBatch);
  $('pie-batch').classList.toggle('pie-on', byBatch);
}
$('pie-files').addEventListener('click', () => { pieMode = 'files'; renderPie(); });
$('pie-batch').addEventListener('click', () => { pieMode = 'batch'; renderPie(); });

function renderTiles() {
  $('t-total').textContent = String(live.total);
  $('t-total-b').textContent = humanBytes(live.totalBytes);
  $('t-done').textContent = String(live.done);
  $('t-done-b').textContent = humanBytes(live.doneBytes);
  $('t-pending').textContent = String(live.pending);
  $('t-pending-b').textContent = humanBytes(live.pendingBytes);
  $('t-skipped').textContent = String(live.skipped);
  $('t-skipped-b').textContent = humanBytes(live.skippedBytes);
  const pct = live.totalBytes > 0 ? Math.min(100, live.doneBytes / live.totalBytes * 100) : 0;
  $('progress-bar').style.width = `${pct.toFixed(1)}%`;
  $('progress-text').textContent = live.pending === 0 && live.total > 0
    ? 'Migración completa: nada pendiente.'
    : `${pct.toFixed(1)}% de los bytes migrados · ${live.done} de ${live.total} ficheros`;
  renderPie();
}

function renderSummary(s: main.Summary) {
  live.total = s.totalFiles;
  live.totalBytes = s.totalBytes;
  live.done = s.done;
  live.doneBytes = s.doneBytes;
  live.pending = s.pending;
  live.pendingBytes = s.pendingBytes;
  live.skipped = s.skipped + (s.tooLarge?.length || 0);
  live.skippedBytes = s.skippedBytes;
  live.quarantined = (s.quarantined || []).length;
  live.quarantinedBytes = s.quarantinedBytes || 0;
  renderTiles();

  let pend = humanBytes(s.pendingBytes);
  if (s.retries || s.changed) pend += ` · ${s.retries} reintentos, ${s.changed} modificados`;
  $('t-pending-b').textContent = pend;
  $('scanned-at').textContent = `escaneado a las ${s.scannedAt}`;
  const tl = $('too-large');
  tl.textContent = s.tooLarge && s.tooLarge.length
    ? `${s.tooLarge.length} ficheros superan el máximo y se excluirán`
    : '';
  tl.title = (s.tooLarge || []).map(f => `${humanBytes(f.size)}  ${f.path}`).join('\n');

  const note = $('placeholders-note');
  if (s.placeholders > 0) {
    note.textContent = `${s.placeholders} de los pendientes son "solo online": se descargarán de la nube de Dropbox (su tamaño no se conoce hasta descargarlos).`
      + (s.emptyUploads > 0 ? ` ${s.emptyUploads} de ellos se subieron vacíos con una versión anterior y vuelven a la cola.` : '');
    note.classList.remove('hidden');
  } else if (s.emptyUploads > 0) {
    note.textContent = `${s.emptyUploads} ficheros "solo online" se subieron vacíos con una versión anterior: vuelven a la cola para subir su contenido real.`;
    note.classList.remove('hidden');
  } else if (s.placeholdersSkipped > 0) {
    note.textContent = `${s.placeholdersSkipped} ficheros "solo online" sin contenido local están fuera de la cola. Conecta Dropbox y activa "Descargar de la nube" para incluirlos.`;
    note.classList.remove('hidden');
  } else {
    note.classList.add('hidden');
  }
  renderQuarantine(s);
}

function renderQuarantine(s: main.Summary) {
  const q = s.quarantined || [];
  const box = $('quarantine-note');
  const btn = $<HTMLButtonElement>('btn-retry-quarantined');
  box.classList.toggle('hidden', q.length === 0);
  btn.classList.toggle('hidden', q.length === 0);
  if (q.length === 0) return;
  btn.textContent = `Reintentar aparcados (${q.length})`;
  $('quarantine-summary').textContent = `${q.length} ficheros aparcados${s.quarantinedBytes ? ' (' + humanBytes(s.quarantinedBytes) + ')' : ''}: fallan con el mismo error lote tras lote y ya no se reintentan solos. Pulsa para ver cuáles.`;
  const list = $('quarantine-list');
  list.textContent = '';
  for (const f of q) {
    const li = document.createElement('li');
    li.textContent = `${f.path}${f.time ? ' · ' + f.time : ''}`;
    li.title = f.error || '';
    li.insertAdjacentHTML('beforeend', skipButton(f.path));
    list.appendChild(li);
  }
}

// Avance del fichero en curso (fases descarga / subida / verificación).
const phaseLabel: Record<string, string> = { download: 'Descargando de Dropbox', wait: 'Esperando turno de subida', upload: 'Subiendo a Internxt', verify: 'Verificando' };

function renderFileProgress(p: { path: string; phase: string; done: number; total: number }) {
  const box = $('file-progress');
  box.classList.remove('hidden');
  const name = p.path.split('/').pop() || p.path;
  const pct = p.total > 0 ? Math.min(100, p.done / p.total * 100) : 0;
  $('file-progress-text').textContent = `${phaseLabel[p.phase] || p.phase}: ${name}`;
  $('file-progress-pct').textContent = p.total > 0
    ? `${humanBytes(p.done)} / ${humanBytes(p.total)} (${pct.toFixed(0)}%)`
    : p.phase === 'wait' ? 'otra subida grande en curso' : humanBytes(p.done);
  $('file-progress-bar').style.width = `${p.total > 0 ? pct : 0}%`;
  const tr = rowByPath.get(p.path);
  if (tr) {
    (tr.children[3] as HTMLElement).textContent = `${phaseLabel[p.phase] || p.phase} ${p.total > 0 ? pct.toFixed(0) + '%' : ''}`;
    // Un fichero en espera o aplazado que empieza a moverse pasa a "en curso".
    const st = tr.children[0].textContent;
    if (st === 'espera' || st === 'aplazado') {
      tr.children[0].innerHTML = '<span class="st st-wait">en curso</span>';
      if (pieMode === 'batch') renderPie();
    }
  }
}

function hideFileProgress() {
  $('file-progress').classList.add('hidden');
}

// ---------- lote ----------

const batchRows = $<HTMLTableSectionElement>('batch-rows');
const rowByPath = new Map<string, HTMLTableRowElement>();

function resetBatchTable() {
  batchRows.innerHTML = '';
  rowByPath.clear();
  $('batch-head').textContent = 'Sin lote cargado.';
  renderPie();
}

function fillBatchTable(rows: main.FileRow[]) {
  resetBatchTable();
  let bytes = 0;
  for (const r of rows) {
    bytes += r.size;
    const tr = document.createElement('tr');
    tr.innerHTML = `<td><span class="st st-wait">espera</span></td><td class="num">${r.size === 0 ? 'nube' : humanBytes(r.size)}</td><td class="path">${escapeHtml(r.path)}</td><td class="detail"></td><td class="act">${skipButton(r.path)}</td>`;
    batchRows.appendChild(tr);
    rowByPath.set(r.path, tr);
  }
  $('batch-head').textContent = rows.length
    ? `Lote: ${rows.length} ficheros, ${humanBytes(bytes)}`
    : 'Nada pendiente.';
  renderPie();
}

function markRow(ev: { path: string; size: number; bytes: number; status: string; error: string; millis: number; attempt: number; index: number; total: number }) {
  // Contadores en vivo.
  if (ev.status === 'done') {
    live.done++;
    live.doneBytes += ev.bytes;
    live.pending = Math.max(0, live.pending - 1);
    live.pendingBytes = Math.max(0, live.pendingBytes - ev.size);
    if (ev.size === 0 && ev.bytes > 0) live.totalBytes += ev.bytes; // marcador: ahora se conoce su tamaño
  } else if (ev.status === 'skipped') {
    live.skipped++;
    live.pending = Math.max(0, live.pending - 1);
    live.pendingBytes = Math.max(0, live.pendingBytes - ev.size);
  }
  renderTiles();

  const tr = rowByPath.get(ev.path);
  if (ev.status === 'deferred') {
    // Sin sitio en disco por ahora: el hilo sigue con otro fichero y este se retoma al final del lote.
    if (tr) {
      tr.children[0].innerHTML = '<span class="st st-skipped">aplazado</span>';
      (tr.children[3] as HTMLElement).textContent = 'sin sitio en disco mientras bajan otros; se retoma al final del lote';
    }
    return;
  }
  $('batch-head').textContent = `Lote en curso${batchStartedAt ? ` desde las ${batchStartedAt}` : ''}: ${ev.index}/${ev.total}`;
  if (!tr) return;
  const cls = ev.status === 'done' ? 'st-done' : ev.status === 'failed' ? 'st-failed' : 'st-skipped';
  const label = ev.status === 'done' ? 'OK' : ev.status === 'failed' ? 'FALLO' : 'omitido';
  tr.children[0].innerHTML = `<span class="st ${cls}">${label}</span>`;
  if (tr.children[4]) tr.children[4].textContent = '';
  if (ev.status === 'done') (tr.children[1] as HTMLElement).textContent = humanBytes(ev.bytes);
  const detail = ev.status === 'done'
    ? `${(ev.millis / 1000).toFixed(1)} s${ev.attempt > 1 ? ` (intento ${ev.attempt})` : ''}`
    : ev.error;
  (tr.children[3] as HTMLElement).textContent = detail;
  tr.scrollIntoView({ block: 'nearest' });
}

// ---------- ocupado / botones ----------

const actionButtons = ['btn-scan', 'btn-preview', 'btn-run', 'btn-run-all', 'btn-retry-quarantined', 'btn-verify', 'btn-save', 'btn-docker-up', 'btn-docker-down'];
let continuous = false;

function setBusy(kind: string) {
  const busy = kind !== '';
  for (const id of actionButtons) $<HTMLButtonElement>(id).disabled = busy;
  $<HTMLButtonElement>('btn-stop').disabled = !busy;
  const after = $<HTMLButtonElement>('btn-stop-after');
  after.classList.toggle('hidden', kind !== 'all');
  if (kind !== 'all') setStopAfter(false);
  const pill = $('busy');
  pill.classList.toggle('hidden', !busy);
  pill.textContent = kind === 'batch' ? 'migrando lote…' : kind === 'all' ? 'migrando todo…' : kind === 'verify' ? 'verificando…' : '';
  pill.className = `pill ${busy ? 'pill-warn' : 'pill-muted'}${busy ? '' : ' hidden'}`;
}

// ---------- fallos ----------

async function refreshFailed() {
  try {
    const rows = await App.ListFailed();
    const body = $<HTMLTableSectionElement>('failed-rows');
    body.innerHTML = '';
    for (const r of rows) {
      const tr = document.createElement('tr');
      tr.innerHTML = `<td class="num">${escapeHtml(r.time)}</td><td class="num">${humanBytes(r.size)}</td><td class="path">${escapeHtml(r.path)}</td><td class="detail">${escapeHtml(r.error)}</td><td class="act">${skipButton(r.path)}</td>`;
      body.appendChild(tr);
    }
    const badge = $('failed-count');
    badge.textContent = String(rows.length);
    badge.classList.toggle('hidden', rows.length === 0);
  } catch (e) {
    logLocal(`error listando fallos: ${errText(e)}`);
  }
}

// ---------- conexión ----------

function setConn(text: string, kind: 'ok' | 'warn' | 'danger' | 'muted') {
  const pill = $('conn-pill');
  pill.textContent = text;
  pill.className = `pill pill-${kind}`;
}

async function testConnection(): Promise<boolean> {
  setConn('comprobando…', 'muted');
  try {
    const msg = await App.TestConnection();
    setConn('WebDAV conectado', 'ok');
    notify(msg, 'ok');
    logLocal(msg);
    return true;
  } catch (e) {
    const err = errText(e);
    setConn('sin conexión', 'danger');
    const hint = /connection refused|dial tcp|no such host|timeout/i.test(err)
      ? 'El servidor WebDAV no responde. Rellena la cuenta Internxt en Configuración, guarda y pulsa "Arrancar WebDAV".'
      : /401/.test(err)
        ? 'El WebDAV rechaza las credenciales: revisa usuario y contraseña del WebDAV en Configuración.'
        : '';
    notify(`Sin conexión: ${err}\n${hint}`.trim(), 'err');
    logLocal(`conexión: ${err}`);
    return false;
  }
}

const dockerButtons = ['btn-docker-up', 'btn-docker-down', 'btn-test'];

async function dockerAction(action: 'up' | 'down') {
  for (const id of dockerButtons) $<HTMLButtonElement>(id).disabled = true;
  try {
    if (action === 'up') {
      notify('Arrancando WebDAV…', 'warn');
      const out = await App.DockerUp();
      notify(out || 'WebDAV en línea', 'ok');
      await testConnection();
    } else {
      notify('Parando el contenedor…', 'warn');
      const out = await App.DockerDown();
      if (out) logLocal(out.split('\n').slice(-3).join(' | '));
      setConn('WebDAV parado', 'muted');
      notify('Contenedor parado.', '');
    }
  } catch (e) {
    notify(`Docker: ${errText(e)}`, 'err');
    logLocal(`docker: ${errText(e)}`);
  } finally {
    for (const id of dockerButtons) $<HTMLButtonElement>(id).disabled = false;
  }
}

// ---------- pestañas ----------

for (const tab of document.querySelectorAll<HTMLButtonElement>('.tab')) {
  tab.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
    tab.classList.add('active');
    for (const body of document.querySelectorAll<HTMLElement>('.tab-body')) {
      body.classList.toggle('hidden', body.id !== `tab-${tab.dataset.tab}`);
    }
    if (tab.dataset.tab === 'failed') void refreshFailed();
  });
}

// ---------- eventos del backend ----------

EventsOn('log', (e: { time: string; msg: string }) => addLog(e.time, e.msg));
EventsOn('summary', (s: main.Summary) => renderSummary(s));
let batchStartedAt = '';
EventsOn('batch:start', (ev: { count: number; bytes: number; files?: main.FileRow[] }) => {
  batchStartedAt = new Date().toLocaleTimeString('es-ES', { hour: '2-digit', minute: '2-digit' });
  setBusy(continuous ? 'all' : 'batch');
  // La tabla se rellena con la lista real del lote: en "Reintentar aparcados"
  // no coincide con la vista previa, y en "Migrar todo" cada lote es distinto.
  if (ev.files && ev.files.length) fillBatchTable(ev.files);
});
EventsOn('file', (ev) => markRow(ev));
EventsOn('file:progress', (p) => renderFileProgress(p));
EventsOn('batch:end', (r: { done: number; failed: number; bytes: number; seconds: number; remaining: number; stopped: boolean; error: string; authLost?: boolean; continuous?: boolean }) => {
  const goesOn = !!r.continuous && !r.error && !r.stopped && r.remaining > 0 && r.done > 0;
  if (!goesOn) { continuous = false; setBusy(''); }
  hideFileProgress();
  renderPie();
  if (goesOn) {
    logLocal(`lote terminado: ${r.done} correctos (${humanBytes(r.bytes)}), ${r.failed} fallidos, quedan ${r.remaining}; sigue el siguiente lote`);
    resetBatchTable();
    $('batch-head').textContent = `Lote terminado: ${r.done} correctos, ${r.failed} fallidos, quedan ${r.remaining}. Preparando el siguiente…`;
    if (r.failed) void refreshFailed();
    return;
  }
  if (r.error) {
    logLocal(`lote abortado: ${r.error}`);
    $('batch-head').textContent = `Lote abortado: ${r.error}`;
    notify(r.authLost ? r.error : `Lote abortado: ${r.error}`, 'err');
  } else {
    $('batch-head').textContent = `Lote terminado${r.stopped ? ' (detenido)' : ''}: ${r.done} correctos (${humanBytes(r.bytes)}), ${r.failed} fallidos, quedan ${r.remaining}`;
  }
  if (r.failed) void refreshFailed();
});
EventsOn('verify:progress', (p: { checked: number }) => { $('batch-head').textContent = `Verificando… ${p.checked} comprobados`; });
EventsOn('verify:fail', (f: { path: string; error: string }) => logLocal(`VERIFY FALLO ${f.path}: ${f.error}`));
EventsOn('verify:end', (r: { ok: number; bad: number; stopped?: boolean; error?: string }) => {
  setBusy('');
  $('batch-head').textContent = r.error
    ? `Verificación abortada: ${r.error}`
    : `Verificación${r.stopped ? ' detenida' : ' terminada'}: ${r.ok} correctos, ${r.bad} con problemas`;
  void refreshFailed();
  void App.Scan().catch(() => undefined);
});
EventsOn('schedule:tick', (t: string) => logLocal(`programación: lanzando lote (${t})`));
EventsOn('docker:step', (msg: string) => notify(msg, 'warn'));
EventsOn('dropbox:auth-lost', async (msg: string) => {
  logLocal(`Dropbox: ${msg}`);
  currentCfg = await App.GetConfig();
  renderDropboxState(currentCfg);
});

// ---------- botones ----------

$('btn-save').addEventListener('click', () => void saveConfig());
$('btn-choose-source').addEventListener('click', async () => {
  const dir = await App.ChooseSourceDir();
  if (dir) input('cfg-source').value = dir;
});
$('btn-choose-compose').addEventListener('click', async () => {
  const dir = await App.ChooseComposeDir();
  if (dir) input('cfg-compose').value = dir;
});
$('btn-test').addEventListener('click', () => void testConnection());
$('btn-docker-up').addEventListener('click', () => void dockerAction('up'));
$('btn-docker-down').addEventListener('click', () => void dockerAction('down'));

$('btn-scan').addEventListener('click', async () => {
  $('progress-text').textContent = 'Escaneando…';
  try {
    renderSummary(await App.Scan());
    resetBatchTable();
  } catch (e) {
    $('progress-text').textContent = `Error: ${errText(e)}`;
  }
});
$('btn-preview').addEventListener('click', async () => {
  try { fillBatchTable(await App.PreviewBatch()); } catch (e) { logLocal(`error: ${errText(e)}`); }
});
$('btn-run').addEventListener('click', async () => {
  try {
    if (rowByPath.size === 0) fillBatchTable(await App.PreviewBatch());
    if (rowByPath.size === 0) { logLocal('nada pendiente'); return; }
    await App.StartBatch();
    setBusy('batch');
  } catch (e) {
    logLocal(`error: ${errText(e)}`);
  }
});
$('btn-verify').addEventListener('click', async () => {
  try {
    resetBatchTable();
    await App.StartVerify(input('verify-hash').checked);
    setBusy('verify');
    $('batch-head').textContent = 'Verificando…';
  } catch (e) {
    logLocal(`error: ${errText(e)}`);
  }
});
$('btn-run-all').addEventListener('click', async () => {
  try {
    if (rowByPath.size === 0) fillBatchTable(await App.PreviewBatch());
    if (rowByPath.size === 0) { logLocal('nada pendiente'); return; }
    continuous = true;
    await App.StartAll();
    setBusy('all');
    notify('Migración continua en marcha: encadena lotes hasta terminar. Deja la app abierta; el Mac no entrará en reposo.', 'warn');
  } catch (e) {
    continuous = false;
    notify(errText(e), 'err');
  }
});
$('btn-retry-quarantined').addEventListener('click', async () => {
  try {
    resetBatchTable();
    await App.RetryQuarantined();
    setBusy('batch');
    notify('Reintentando los ficheros aparcados. Si vuelven a fallar, quedan aparcados de nuevo.', 'warn');
  } catch (e) {
    notify(errText(e), 'err');
  }
});
EventsOn('all:end', (r: { batches: number; stopped: boolean }) => {
  continuous = false;
  setBusy('');
  notify(r.stopped ? `Migración continua detenida tras ${r.batches} lotes.` : `Migración continua terminada tras ${r.batches} lotes.`, r.stopped ? 'warn' : 'ok');
});
$('btn-stop').addEventListener('click', () => void App.Stop());

// ---------- saltar ficheros ----------

// Botón "saltar" de una fila: excluye el fichero para siempre (patrón exacto
// en Exclusiones) y, si está en el lote en curso, lo retira o lo interrumpe.
function skipButton(path: string): string {
  return `<button class="btn btn-mini" data-skip="${escapeHtml(path)}" title="No migrar este fichero: se añade a Exclusiones y, si está en curso, se interrumpe">saltar</button>`;
}

document.addEventListener('click', async (e) => {
  const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('button[data-skip]');
  if (!btn) return;
  const path = btn.dataset.skip || '';
  if (!path) return;
  btn.disabled = true;
  try {
    currentCfg = await App.SkipFile(path);
    $<HTMLTextAreaElement>('cfg-excludes').value = (currentCfg.excludes || []).join('\n');
    const tr = rowByPath.get(path);
    if (tr && tr.parentElement === batchRows) {
      const st = tr.children[0].textContent;
      if (st === 'espera' || st === 'aplazado' || st === 'en curso') {
        tr.children[0].innerHTML = '<span class="st st-skipped">omitido</span>';
        (tr.children[3] as HTMLElement).textContent = 'saltado por ti; excluido de los próximos lotes';
      }
      if (tr.children[4]) tr.children[4].textContent = '';
      if (pieMode === 'batch') renderPie();
    } else {
      btn.remove();
    }
    logLocal(`saltado: ${path} (añadido a Exclusiones)`);
  } catch (err) {
    btn.disabled = false;
    notify(errText(err), 'err');
  }
});

// ---------- gestión de exclusiones ----------

// La ventana trabaja sobre una copia: quitar y añadir se anotan en la lista y
// solo se aplican al pulsar "Guardar". Así se puede deshacer y revisar antes.
type ExclEntry = { row: main.ExcludeRow; removed: boolean; added: boolean };
let exclEntries: ExclEntry[] = [];
let exclOriginal: string[] = [];
const exclModal = $<HTMLDialogElement>('excl-modal');

const kindLabel: Record<string, string> = { fichero: 'fichero', carpeta: 'carpeta', nombre: 'nombre', patrón: 'patrón' };

function exclChanged(): number {
  return exclEntries.filter(e => e.removed || e.added).length;
}

function exclDisplay(r: main.ExcludeRow): string {
  // Las rutas exactas se enseñan sin el anclaje ni los escapes del patrón.
  return r.target || r.pattern;
}

function exclEffect(r: main.ExcludeRow): string {
  if (r.matches === 0) return r.target && !r.exists ? 'nada: ya no existe en Dropbox' : 'nada';
  return `${r.matches} ${r.matches === 1 ? 'fichero' : 'ficheros'}${r.bytes ? ' · ' + humanBytes(r.bytes) : ''}`;
}

function exclJournal(r: main.ExcludeRow): string {
  if (!r.target) return '';
  if (!r.status) return 'sin registro';
  const what = r.status === 'done' ? 'migrado' : r.status === 'failed' ? 'falló' : r.status === 'skipped' ? 'omitido' : r.status;
  return `${what}${r.time ? ' el ' + r.time : ''}`;
}

function renderExclusions() {
  const filter = input('excl-filter').value.trim().toLowerCase();
  const kind = $<HTMLSelectElement>('excl-kind').value;
  const body = $<HTMLTableSectionElement>('excl-rows');
  body.innerHTML = '';
  let shown = 0;
  exclEntries.forEach((e, i) => {
    const r = e.row;
    if (kind && r.kind !== kind) return;
    if (filter && !exclDisplay(r).toLowerCase().includes(filter) && !r.pattern.toLowerCase().includes(filter)) return;
    shown++;
    const tr = document.createElement('tr');
    tr.className = e.removed ? 'removed' : e.added ? 'added' : '';
    const sub = r.target && r.pattern !== '/' + r.target ? `<span class="sub">${escapeHtml(r.pattern)}</span>` : '';
    const j = exclJournal(r);
    const jcls = r.status === 'failed' ? 'err' : (r.target && !r.exists) ? 'warn' : '';
    tr.innerHTML = `<td>${escapeHtml(kindLabel[r.kind] || r.kind)}</td>`
      + `<td class="pat" title="${escapeHtml((r.samples || []).join('\n'))}">${escapeHtml(exclDisplay(r))}${sub}</td>`
      + `<td class="num">${escapeHtml(exclEffect(r))}</td>`
      + `<td class="detail"><span class="${jcls}" title="${escapeHtml(r.error || '')}">${escapeHtml(j)}</span></td>`
      + `<td class="act"><button class="btn btn-mini" data-excl="${i}">${e.removed ? 'deshacer' : 'quitar'}</button></td>`;
    body.appendChild(tr);
  });
  $('excl-empty').classList.toggle('hidden', exclEntries.length > 0);
  const kept = exclEntries.filter(e => !e.removed);
  const files = kept.reduce((a, e) => a + e.row.matches, 0);
  const n = exclChanged();
  $('excl-summary').textContent = `${kept.length} exclusiones${shown !== exclEntries.length ? ` (${shown} en pantalla)` : ''} · dejan fuera hasta ${files} ficheros`
    + (n ? ` · ${n} cambios sin guardar` : '');
  const save = $<HTMLButtonElement>('excl-save');
  save.disabled = n === 0;
  save.textContent = n ? `Guardar (${n})` : 'Guardar';
  const useless = kept.filter(e => e.row.matches === 0).length;
  const dropUseless = $<HTMLButtonElement>('excl-drop-useless');
  dropUseless.disabled = useless === 0;
  dropUseless.textContent = `Quitar sin efecto${useless ? ` (${useless})` : ''}`;
  $<HTMLButtonElement>('excl-drop-all').disabled = kept.length === 0;
}

async function openExclusions() {
  exclEntries = [];
  exclOriginal = [...(currentCfg.excludes || [])];
  $('excl-summary').textContent = 'Recorriendo la carpeta de Dropbox para medir cada exclusión…';
  $('excl-rows').innerHTML = '';
  $('excl-preview').textContent = '';
  $('excl-preview').className = 'hint';
  $('excl-msg').textContent = '';
  input('excl-new').value = '';
  if (!exclModal.open) exclModal.showModal();
  try {
    const rep = await App.ExcludeReport();
    exclEntries = rep.rows.map(row => ({ row, removed: false, added: false }));
    $('excl-defaults').textContent = rep.defaults.map(d => d === 'Icon\r' ? 'Icon\\r' : d).join('   ');
    renderExclusions();
    $('excl-summary').textContent += ` · ${rep.excluded} de ${rep.files} ficheros del origen quedan fuera (${humanBytes(rep.excludedBytes)})`;
  } catch (e) {
    $('excl-summary').textContent = `Error: ${errText(e)}`;
  }
}

function closeExclusions() {
  exclModal.close();
}

async function testExclusion(add: boolean) {
  const pat = input('excl-new').value.trim();
  const out = $('excl-preview');
  if (!pat) { out.textContent = 'Escribe un patrón.'; out.className = 'hint err'; return; }
  const dup = exclEntries.find(e => e.row.pattern === pat && !e.removed);
  if (dup) { out.textContent = 'Ese patrón ya está en la lista.'; out.className = 'hint err'; return; }
  out.textContent = 'Probando…'; out.className = 'hint';
  try {
    const row = await App.TestExclude(pat);
    const sample = row.samples && row.samples.length ? ` Por ejemplo: ${row.samples.slice(0, 3).join(' · ')}` : '';
    out.textContent = `${kindLabel[row.kind] || row.kind}: deja fuera ${exclEffect(row)}.${sample}`;
    out.className = row.matches ? 'hint ok' : 'hint';
    if (add) {
      const undone = exclEntries.find(e => e.row.pattern === pat && e.removed);
      if (undone) { undone.removed = false; undone.row = row; } else exclEntries.push({ row, removed: false, added: true });
      input('excl-new').value = '';
      renderExclusions();
    }
  } catch (e) {
    out.textContent = errText(e); out.className = 'hint err';
  }
}

async function saveExclusions() {
  const list = exclEntries.filter(e => !e.removed).map(e => e.row.pattern);
  const btn = $<HTMLButtonElement>('excl-save');
  btn.disabled = true;
  try {
    currentCfg = await App.SaveExcludes(list);
    $<HTMLTextAreaElement>('cfg-excludes').value = (currentCfg.excludes || []).join('\n');
    const removed = exclOriginal.filter(p => !list.includes(p)).length;
    const added = list.filter(p => !exclOriginal.includes(p)).length;
    closeExclusions();
    logLocal(`exclusiones guardadas: ${list.length} (${added} nuevas, ${removed} quitadas)`);
    notify(`Exclusiones guardadas: ${added} nuevas, ${removed} quitadas. Lo que deja de estar excluido vuelve a la cola en el próximo escaneo.`, 'ok');
    resetBatchTable();
    renderSummary(await App.Scan());
  } catch (e) {
    $('excl-msg').textContent = errText(e);
    $('excl-msg').className = 'msg err';
    btn.disabled = false;
  }
}

$('btn-excludes').addEventListener('click', () => void openExclusions());
$('tile-skipped').addEventListener('click', () => void openExclusions());
$('tile-skipped').addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); void openExclusions(); } });
$('excl-close').addEventListener('click', closeExclusions);
$('excl-cancel').addEventListener('click', closeExclusions);
$('excl-save').addEventListener('click', () => void saveExclusions());
$('excl-test').addEventListener('click', () => void testExclusion(false));
$('excl-addbtn').addEventListener('click', () => void testExclusion(true));
input('excl-new').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); void testExclusion(true); } });
input('excl-filter').addEventListener('input', renderExclusions);
$('excl-kind').addEventListener('change', renderExclusions);
$('excl-drop-useless').addEventListener('click', () => {
  for (const e of exclEntries) if (!e.removed && e.row.matches === 0) e.removed = true;
  renderExclusions();
});
$('excl-drop-all').addEventListener('click', () => {
  for (const e of exclEntries) e.removed = true;
  renderExclusions();
});
$('excl-rows').addEventListener('click', (e) => {
  const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('button[data-excl]');
  if (!btn) return;
  const entry = exclEntries[Number(btn.dataset.excl)];
  if (!entry) return;
  if (entry.added && !entry.removed) exclEntries.splice(exclEntries.indexOf(entry), 1); // una alta sin guardar se descarta sin más
  else entry.removed = !entry.removed;
  renderExclusions();
});
// Esc no descarta cambios por accidente: con cambios pendientes se avisa en el
// pie y hay que elegir Guardar o Cancelar. (No se usa confirm(): el WebView no lo muestra.)
exclModal.addEventListener('cancel', (e) => {
  if (exclChanged() === 0) return;
  e.preventDefault();
  $('excl-msg').textContent = 'Hay cambios sin guardar: pulsa Guardar para aplicarlos o Cancelar para descartarlos.';
  $('excl-msg').className = 'msg err';
});

// "Parar al acabar el lote": el lote en curso se termina entero y no se encadena otro.
function setStopAfter(armed: boolean) {
  const b = $<HTMLButtonElement>('btn-stop-after');
  b.classList.toggle('armed', armed);
  b.textContent = armed ? 'Parará al acabar el lote (pulsa para seguir)' : 'Parar al acabar el lote';
}
$('btn-stop-after').addEventListener('click', async () => {
  try {
    const armed = await App.StopAfterBatch();
    setStopAfter(armed);
    notify(armed ? 'Se parará al acabar el lote en curso. Puedes cerrar la app o reiniciar cuando la tarta del lote llegue al 100 %.' : 'Se seguirá encadenando lotes.', armed ? 'warn' : 'ok');
  } catch (e) {
    notify(errText(e), 'err');
  }
});
$('btn-refresh-failed').addEventListener('click', () => void refreshFailed());

$('btn-dbx-connect').addEventListener('click', async () => {
  const key = input('cfg-dbx-key').value.trim();
  if (!key) { notify('Introduce la App key de tu aplicación de Dropbox.', 'err'); return; }
  notify('Se ha abierto el navegador: autoriza la aplicación en Dropbox y vuelve aquí.', 'warn');
  $<HTMLButtonElement>('btn-dbx-connect').disabled = true;
  try {
    const msg = await App.DropboxConnect(key);
    currentCfg = await App.GetConfig();
    cfgToForm(currentCfg);
    notify(msg, 'ok');
    renderSummary(await App.Scan());
  } catch (e) {
    notify(`Dropbox: ${errText(e)}`, 'err');
  } finally {
    $<HTMLButtonElement>('btn-dbx-connect').disabled = false;
  }
});
$('btn-dbx-disconnect').addEventListener('click', async () => {
  try {
    await App.DropboxDisconnect();
    currentCfg = await App.GetConfig();
    cfgToForm(currentCfg);
    notify('Dropbox desconectado.', '');
    renderSummary(await App.Scan());
  } catch (e) {
    notify(`Dropbox: ${errText(e)}`, 'err');
  }
});

async function refreshDropboxStatus() {
  if (!currentCfg.dropboxRefreshToken) return;
  try {
    $('dbx-status').textContent = await App.DropboxStatus();
  } catch (e) {
    $('dbx-status').textContent = errText(e);
  }
}

// ---------- arranque ----------

(async () => {
  currentCfg = await App.GetConfig();
  cfgToForm(currentCfg);
  $('dbx-redirect').textContent = await App.DropboxRedirectURI();
  const busy = await App.IsBusy();
  setBusy(busy);
  logLocal('aplicación lista');
  void testConnection();
  void refreshDropboxStatus();
  try { renderSummary(await App.Scan()); } catch (e) { $('progress-text').textContent = `Error al escanear: ${errText(e)}`; }
})();
