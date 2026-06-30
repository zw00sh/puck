"use strict";
// puckview dashboard — live data over SSE, mutations over fetch. The server is
// the source of truth: every action POSTs/DELETEs and the resulting `devices`
// SSE frame re-renders the table. Only transient UI state (which rows are in
// edit mode, the draft row, the active tab, open modals) lives on the client.

const $ = s => document.querySelector(s);
const clamp = (v, a, b) => Math.max(a, Math.min(b, v));
const thr = (v, w, b) => v >= b ? 'bad' : v >= w ? 'warn' : 'ok';
const esc = s => String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

function bar(p, w, t) {
  const n = clamp(Math.round(p / 100 * w), 0, w);
  const c = t === 'neutral' ? '#8a93a3' : t === 'bad' ? 'var(--bad)' : t === 'warn' ? 'var(--warn)' : 'var(--ok)';
  return `<span style="color:${c}">${'█'.repeat(n)}</span><span class="dimmer">${'░'.repeat(w - n)}</span>`;
}
function hist(a, m) { const ch = ' ▁▂▃▄▅▆▇█'; m = m || 1; return a.map(v => ch[clamp(Math.round(v / m * 8), 0, 8)]).join(''); }
function fmtUptime(s) {
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  return `${d}d ${String(h).padStart(2, '0')}h ${String(m).padStart(2, '0')}m`;
}

// ---------- client-only UI state ----------
let tracked = [], cache = [];
let editing = new Set();   // macs in edit mode
let adding = false;        // draft row visible
let discView = 'cache';
let wakeMac = null;        // mac of the device shown in the wake modal
let wakeDone = false;      // wake finished (success / timeout / stopped)
let wakeStopped = false;   // user pressed [stop]
let lastWake = null;       // most recent wake SSE event, for re-render
const devByMac = m => tracked.find(d => d.mac === m);

// ---------- cells ----------
function arpCell(s) { const m = { up: ['ok', 'OK'], stale: ['warn', 'STALE'], down: ['bad', 'FAIL'] }; const [c, t] = m[s] || m.down; return `<span class="${c}">${t}</span>`; }
// ICMP reply = liveness, coloured by latency (green ≤10ms, amber ≤30, red 30+);
// no reply → dim dash.
function icmpCell(ic) {
  if (!ic || ic.st !== 'up') return '<span class="dim">-</span>';
  const rtt = ic.rtt ?? 0;
  return `<span class="${thr(rtt, 11, 31)}">${rtt}ms</span>`;
}
// Single source of truth for a device's liveness, shared by the status dot, the
// attention line, and the "N up" counter so they never disagree:
//   up      — ICMP reply or a TCP watchdog open (the OS/services answered)
//   suspect — only an ARP entry, but we'd expect more (has watchdogs, or has
//             answered ICMP before) → likely asleep (NIC ARP-offload)
//   present — only an ARP entry, nothing more expected of it (just on the LAN)
//   down    — no signal at all
function liveState(d) {
  const icmpUp = d.icmp && d.icmp.st === 'up';
  const tcpUp = (d.probes || []).some(p => p.st === 'up');
  if (icmpUp || tcpUp) return 'up';
  if (d.arp === 'up' || d.arp === 'stale') {
    return ((d.probes || []).length > 0 || d.icmpSeen) ? 'suspect' : 'present';
  }
  return 'down';
}
// SEEN cell for a tracked host: time since last confirmed UP (ICMP/TCP). When
// the value is an ARP-presence fallback (never verified up), mark it with ~ and a
// tooltip so it's clearly weaker than a real sighting.
function seenCell(d) {
  if (d.seenL2 && d.seen !== '—') return `<span class="dimmer" title="ARP (L2) presence — never verified up">~${esc(d.seen)}</span>`;
  return `<span class="dim">${esc(d.seen)}</span>`;
}
// Leading ST column dot. ARP-only is yellow by default, red when we expected the
// host to answer ICMP/TCP (see liveState).
function liveDot(d) {
  switch (liveState(d)) {
    case 'up': return '<span class="ok" title="up">●</span>';
    case 'present': return '<span class="warn" title="present (ARP only)">◐</span>';
    case 'suspect': return '<span class="bad" title="ARP only — expected ICMP/TCP (likely asleep)">◐</span>';
    default: return '<span class="bad" title="down">○</span>';
  }
}

// ---------- tracked ----------
// Compact watchdog row: ● tcp:80 / open / 2ms  (no phantom column alignment).
function probeRow(mac, p, ed) {
  const up = p.st === 'up';
  const lbl = ed
    ? `tcp:<input class="pin" value="${p.port}" inputmode="numeric" onchange="changePort('${mac}',${p.port},this.value)">`
    : `tcp:${p.port}`;
  const res = up ? '<span class="ok">open</span>' : `<span class="bad">${esc(p.note || 'closed')}</span>`;
  const sep = '<span class="psep">/</span>';
  const rtt = up ? ` ${sep} <span class="dim">${p.rtt ?? 0}ms</span>` : '';
  const del = ed ? ` <span class="cmd danger" onclick="rmProbe('${mac}',${p.port})">[×]</span>` : '';
  return `<div class="probe"><span class="pdot ${up ? 'ok' : 'bad'}">${up ? '●' : '○'}</span>` +
    `<span>${lbl}</span> ${sep} ${res}${rtt}${del}</div>`;
}
function trackedRow(d) {
  const ed = editing.has(d.mac);
  const probes = d.probes || [];
  const tcp = probes.map(p => probeRow(d.mac, p, ed)).join('');
  const editrow = ed ? `
    <div class="addrow"><span class="pdot"></span><span>tcp:<input id="ap_${d.mac}" placeholder="port" inputmode="numeric" class="pin"></span><span class="cmd" onclick="addProbe('${d.mac}')">[add]</span></div>
    <div class="addrow"><span class="sp"></span><span class="cmd danger" onclick="untrack('${d.mac}')">[untrack]</span></div>` : '';
  const probesBlock = (ed || probes.length) ? `<div class="probes ${ed ? 'editing' : ''}">${tcp}${editrow}</div>` : '';
  const nameCell = ed
    ? `<input class="rin" value="${esc(d.name)}" maxlength="32" placeholder="name" onchange="renameDev('${d.mac}', this.value)">`
    : `<span class="nm ell">${esc(d.name)}</span>`;
  return `
  <div class="dev-row t-grid">
    <span class="col-c">${liveDot(d)}</span>
    ${nameCell}<span class="ell">${esc(d.ip)}</span>
    <span class="dim ell">${esc(d.mac)}</span><span class="dim ell">${esc(d.vendor)}</span>
    <span>${seenCell(d)}</span>
    <span>${arpCell(d.arp)}</span><span>${icmpCell(d.icmp)}</span>
    <span class="col-c cmd" onclick="wake('${d.mac}')">[wake]</span>
    <span class="col-c cmd" onclick="toggleEdit('${d.mac}')">${ed ? '[done]' : '[edit]'}</span>
  </div>${probesBlock}`;
}
// Add-device draft: a form row with inputs that flow, and the actions grouped at
// the right ([done] then [×]) rather than borrowing the WAKE/EDIT columns.
function draftRow() {
  return `<div class="dev-row drow" style="display:flex; gap:.5rem; align-items:center;">
    <span class="col-c dim" style="width:24px">+</span>
    <input id="naName" placeholder="name (opt)" maxlength="32" style="flex:1; min-width:78px">
    <input id="naIp" placeholder="192.168.x.x" style="flex:1; min-width:96px">
    <input id="naMac" placeholder="aa:bb:cc:dd:ee:ff" style="flex:1.3; min-width:120px">
    <span class="sp"></span>
    <span class="cmd" onclick="commitAdd()">[done]</span>
    <span class="cmd danger" onclick="cancelAdd()">[×]</span>
  </div>`;
}
function renderAttention() {
  // Flag the genuinely-not-up: down (no signal) and suspect (expected more than
  // the bare ARP entry it's showing — likely asleep). Debounced: only once it
  // has been unseen (not confirmed up) for ≥60s, so a single missed probe or a
  // brief blip doesn't flap the banner.
  const flag = tracked.filter(d => {
    const s = liveState(d);
    return (s === 'down' || s === 'suspect') && (d.unseenSec ?? 0) >= 60;
  });
  const el = $('#attention');
  if (!flag.length) { el.hidden = true; el.innerHTML = ''; return; }
  el.hidden = false;
  el.innerHTML = `<span class="att-tag">DOWN</span>` + flag.map(d =>
    `<span class="att-item">${esc(d.name)} <span class="cmd" onclick="wake('${d.mac}')">[wake]</span></span>`).join('<span class="dim">·</span>');
}
function renderTracked() {
  const body = (adding ? draftRow() : '') + (tracked.length || adding ? tracked.map(trackedRow).join('') : '<div class="empty">— no tracked devices —</div>');
  $('#tracked').innerHTML = body;
  $('#trkN').textContent = `${tracked.length} watched · ${tracked.filter(d => liveState(d) === 'up').length} up`;
  renderAttention();
  if (adding) { const e = $('#naName'); if (e) e.focus(); }
}

// ---------- cache ----------
function cacheRow(c) {
  return `<div class="dev-row c-grid">
    <span>${arpCell(c.state)}</span>
    <span class="nm ell dim">${esc(c.name || '—')}</span><span class="ell">${esc(c.ip)}</span>
    <span class="dim ell">${esc(c.mac)}</span><span class="dim ell">${esc(c.vendor || '—')}</span>
    <span class="dim">${esc(c.seen)}</span>
    <span class="col-c cmd" onclick="track('${c.mac}','${c.ip}','${esc(c.name || '')}')">[track]</span>
  </div>`;
}
function renderCache() {
  $('#cache').innerHTML = cache.length ? cache.map(cacheRow).join('') : '<div class="empty">— neighbour cache empty —</div>';
}

// ---------- fetch helpers ----------
async function api(method, url, body) {
  const opt = { method };
  if (body !== undefined) { opt.headers = { 'Content-Type': 'application/json' }; opt.body = JSON.stringify(body); }
  return fetch(url, opt);
}
const encMac = m => encodeURIComponent(m);

// ---------- actions (thin; SSE re-renders) ----------
window.toggleEdit = m => { if (editing.has(m)) editing.delete(m); else editing.add(m); renderTracked(); };
// getElementById (not querySelector) — the id contains the MAC, whose colons are
// invalid in a CSS selector.
window.addProbe = m => { const el = document.getElementById('ap_' + m); const v = parseInt(el && el.value, 10); if (!v || v < 1 || v > 65535) return; api('POST', `/api/devices/${encMac(m)}/probes`, { port: v }); };
window.rmProbe = (m, port) => api('DELETE', `/api/devices/${encMac(m)}/probes/${port}`);
window.changePort = (m, oldPort, v) => { const n = parseInt(v, 10); if (!n || n === oldPort) return; api('DELETE', `/api/devices/${encMac(m)}/probes/${oldPort}`).then(() => api('POST', `/api/devices/${encMac(m)}/probes`, { port: n })); };
window.untrack = m => { editing.delete(m); api('DELETE', `/api/devices/${encMac(m)}`); };
window.track = (mac, ip, name) => api('POST', '/api/devices', { mac, ip, name });
// Rename a tracked device; the server marks it manual so background rDNS won't
// clobber it, and caps the length (mirrors the input's maxlength).
window.renameDev = (m, v) => api('PATCH', `/api/devices/${encMac(m)}`, { name: v.trim() });

// manual add (draft row + validation)
const MAC_RE = /^([0-9a-fA-F]{2}[:\-]){5}[0-9a-fA-F]{2}$/;
function validIp(s) { const m = s.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/); return !!m && m.slice(1).every(o => +o >= 0 && +o <= 255); }
window.startAdd = () => { adding = true; renderTracked(); };
window.cancelAdd = () => { adding = false; renderTracked(); };
window.commitAdd = async () => {
  const name = $('#naName').value.trim(), ip = $('#naIp').value.trim(), mac = $('#naMac').value.trim();
  const macOk = MAC_RE.test(mac) && !tracked.some(d => d.mac.toLowerCase() === mac.toLowerCase());
  const ipOk = validIp(ip);
  $('#naMac').classList.toggle('err', !macOk); $('#naIp').classList.toggle('err', !ipOk);
  if (!macOk || !ipOk) return;
  // manual:true marks a user-typed name as sticky so background rDNS won't clobber it.
  const r = await api('POST', '/api/devices', { mac: mac.toLowerCase(), ip, name, manual: true });
  if (r.ok) { adding = false; renderTracked(); }
};

// ---------- modal ----------
function openModal(title) {
  $('#modalTitle').textContent = title;
  $('#modalBody').innerHTML = '';
  $('#modalX').textContent = '[close]'; // default; wake() switches it to [stop]
  $('#modal').hidden = false;
}
function closeModal() {
  if (wakeMac && !wakeDone) api('DELETE', `/api/devices/${encMac(wakeMac)}/wake`); // stop the barrage
  $('#modal').hidden = true;
  wakeMac = null; wakeDone = false; wakeStopped = false; lastWake = null;
}
// The wake dialog button is two-phase: [stop] halts the barrage (but keeps the
// dialog open so you can read the result), then becomes [close]. It auto-flips to
// [close] the moment a real signal lands or the attempt finishes/times out.
function setModalX() { $('#modalX').textContent = (wakeMac && !wakeDone) ? '[stop]' : '[close]'; }
$('#modalX').onclick = () => {
  if (wakeMac && !wakeDone) {                                   // phase 1: stop
    api('DELETE', `/api/devices/${encMac(wakeMac)}/wake`);
    wakeStopped = true; wakeDone = true; setModalX(); renderWake();
    return;
  }
  closeModal();                                                 // phase 2: close
};
$('#modal').onclick = e => { if (e.target === $('#modal')) closeModal(); };

// wake — POST then watch the `wake` SSE event for this mac
const wrow = (l, v, c) => `<div class="wrow"><span class="wl">${l}</span><span class="${c || ''}">${v}</span></div>`;
window.wake = m => {
  const d = devByMac(m);
  wakeMac = m; wakeDone = false; wakeStopped = false; lastWake = null;
  openModal('WAKE · ' + (d ? d.name : m));
  setModalX(); // [stop]
  $('#modalBody').innerHTML = `<div class="dim">elapsed <span class="fg">0s</span> <span class="dimmer">· sending…</span></div>`;
  api('POST', `/api/devices/${encMac(m)}/wake`);
};
function paintWake(w) {
  if (w.mac !== wakeMac) return;
  lastWake = w;
  if (w.up || w.done) wakeDone = true; // any ICMP/TCP signal (or timeout) ends it
  setModalX();
  renderWake();
}
// Renders from lastWake. ARP is shown for diagnostics but coloured amber, never
// green — it is not treated as "awake" (NIC offload can ARP-reply while asleep).
function renderWake() {
  const w = lastWake;
  if (!w) return;
  const timeout = w.timeout || 120;
  const pct = w.up ? 100 : clamp(Math.round((w.elapsed || 0) / timeout * 100), 0, 100);
  const rows = [];
  rows.push(wrow('ARP', w.arp === 'up' ? 'reachable' : w.arp === 'stale' ? 'stale' : 'waiting', w.arp === 'down' ? 'dim' : 'warn'));
  rows.push(w.icmp && w.icmp.st === 'up' ? wrow('ICMP', `reply ${w.icmp.rtt ?? 0}ms`, 'ok') : wrow('ICMP', 'waiting', 'dim'));
  (w.probes || []).forEach(p => rows.push(p.st === 'up' ? wrow('tcp:' + p.port, `open ${p.rtt ?? 0}ms`, 'ok') : wrow('tcp:' + p.port, 'waiting', 'dim')));
  if (!(w.probes || []).length) rows.push(wrow('tcp', 'none configured', 'dim'));
  let status;
  if (w.up) status = '<span class="ok">· awake</span>';
  else if (wakeStopped) status = '<span class="warn">· stopped</span>';
  else if (w.done) status = '<span class="bad">· no response</span>';
  else status = '<span class="dimmer">· sending…</span>';
  $('#modalBody').innerHTML =
    `<div class="dim">elapsed <span class="fg">${w.elapsed}s</span> ${status} <span class="dimmer">· ${w.packets} packets</span></div>` +
    `<div class="bar" style="margin:.45rem 0">${bar(pct, 32, 'neutral')}  <span class="dim">${pct}%</span></div>` +
    `<div class="wsep"></div><div class="wstat">${rows.join('')}</div>`;
}

// ---------- discovery tab ----------
function setDisc(v) {
  discView = v;
  $('#cacheView').hidden = v !== 'cache'; $('#scanView').hidden = v !== 'scan';
  $('#discCap').textContent = v === 'cache' ? 'ARP CACHE' : 'SCAN';
  $('#discTab').textContent = v === 'cache' ? '[scan]' : '[arp cache]';
}
$('#discTab').onclick = () => setDisc(discView === 'cache' ? 'scan' : 'cache');
$('#addDevBtn').onclick = startAdd;

// scan — modal progress bar, driven by `scan` SSE events
$('#runScan').onclick = () => {
  const cidr = $('#scanCidr').value.trim();
  openModal('SCAN · ' + cidr);
  $('#modalBody').innerHTML = '<div class="bar" id="modalBar"></div><div class="dim" id="modalStat" style="margin-top:.55rem">starting…</div>';
  api('POST', '/api/scan', { cidr });
};
function paintScan(s) {
  const bel = $('#modalBar'), sel = $('#modalStat');
  if (bel) bel.innerHTML = bar(s.pct || 0, 34, 'neutral') + `  <span class="dim">${s.pct || 0}%</span>`;
  if (sel) sel.textContent = s.msg || '';
  if (s.state === 'done') {
    renderScanResults(s.results || []);
    setTimeout(() => { if (!$('#modal').hidden && $('#modalTitle').textContent.startsWith('SCAN')) closeModal(); }, 1100);
  }
}
function renderScanResults(results) {
  $('#scanRes').innerHTML = results.length ? results.map(c => `
    <div class="dev-row s-grid"><span>${arpCell(c.state)}</span><span class="ell">${esc(c.ip)}</span>
    <span class="dim ell">${esc(c.mac)}</span><span class="dim ell">${esc(c.vendor || '—')}</span>
    <span class="dim">new</span><span class="col-c cmd" onclick="track('${c.mac}','${c.ip}','')">[track]</span></div>`).join('')
    : '<span class="dim">no new hosts</span>';
}

// ---------- stats / hello rendering ----------
function renderHello(h) {
  document.title = (h.host || 'puckview');
  $('#host').textContent = h.host || 'puckview';
  $('#ver').textContent = h.version || 'dev';
  const n = h.net || {};
  if (n.iface) { $('#netIf').textContent = n.iface; $('#ethifLabel').textContent = n.iface; }
  $('#ethip').textContent = n.ip || '—';
  $('#tsip').textContent = n.ts_ip || '—';
  if ($('#scanCidr') && !$('#scanCidr').value) $('#scanCidr').value = n.cidr || '';
  $('#tsState').innerHTML = n.ts_ip ? '<span class="ok">Running</span>' : '<span class="dim">not enrolled</span>';
  $('#tsName').innerHTML = `${esc(h.host || '—')}`;
  $('#tsDns').textContent = (n.dns || []).join(', ') || '—';
}
function renderStats(st) {
  $('#uptime').textContent = fmtUptime(st.uptimeSec || 0);
  const cpu = st.cpu || {}, c = Math.round(cpu.pct || 0), ct = thr(c, 60, 85);
  $('#cpuBar').innerHTML = bar(c, 22, ct); $('#cpuNum').innerHTML = `<span class="${ct}">${c}%</span>`;
  $('#cores').innerHTML = (cpu.cores || []).map((v, i) => { const t = thr(v, 60, 85); return `<div class="m"><span class="lbl">core${i}</span><span class="bar">${bar(v, 10, t)}</span><span class="num ${t}">${Math.round(v)}%</span></div>`; }).join('');
  $('#loadNum').textContent = (cpu.load || [0, 0, 0]).map(x => x.toFixed(2)).join(' ');
  const t = Math.round(cpu.temp || 0), tt = thr(t, 65, 80);
  $('#tempBar').innerHTML = bar(t, 22, tt); $('#tempNum').innerHTML = t ? `<span class="${tt}">${t}°C</span>` : '<span class="dim">n/a</span>';

  const mem = st.mem || {}, m = Math.round(mem.pct || 0), mt = thr(m, 70, 90);
  $('#memBar').innerHTML = bar(m, 22, mt); $('#memNum').innerHTML = `<span class="${mt}">${m}%</span>  ${fmtMB(mem.usedMB)}/${fmtMB(mem.totalMB)}`;
  const sw = st.swap || {};
  if (sw.disabled) { $('#swapBar').innerHTML = bar(0, 22, 'ok'); $('#swapNum').innerHTML = '<span class="dim">off</span>'; }
  else { const s = Math.round(sw.pct || 0), st2 = thr(s, 50, 80); $('#swapBar').innerHTML = bar(s, 22, st2); $('#swapNum').innerHTML = `<span class="${st2}">${s}%</span>  ${fmtMB(sw.usedMB)}/${fmtMB(sw.totalMB)}`; }
  const dk = st.disk || {}, dpct = Math.round(dk.pct || 0), dt = thr(dpct, 80, 92);
  $('#diskBar').innerHTML = bar(dpct, 22, dt); $('#diskNum').innerHTML = `<span class="${dt}">${dpct}%</span>  ${(dk.usedGB || 0).toFixed(0)}/${(dk.totalGB || 0).toFixed(0)}G`;

  const net = st.net || {}; const mx = Math.max(1, ...(net.rx || []), ...(net.tx || []));
  $('#rxBar').textContent = hist(net.rx || [], mx); $('#rxNum').textContent = fmtRate(net.rxKBs);
  $('#txBar').textContent = hist(net.tx || [], mx); $('#txNum').textContent = fmtRate(net.txKBs);

  if (st.ts) {
    const ts = st.ts, ok = ts.state === 'Running';
    $('#tsState').innerHTML = `<span class="${ok ? 'ok' : 'warn'}">${esc(ts.state || '—')}</span>`;
    $('#tsName').innerHTML = esc(ts.name || '—');
    $('#tsMagic').innerHTML = ts.magicdns ? '<span class="ok">ON</span>' : '<span class="dim">off</span>';
    $('#tsExit').innerHTML = ts.exit_node ? '<span class="ok">ON</span>' : '<span class="dim">off</span>';
  }
}
function fmtMB(mb) { mb = mb || 0; return mb >= 1024 ? (mb / 1024).toFixed(1) + 'G' : mb + 'M'; }
function fmtRate(k) { k = k || 0; return k >= 1024 ? (k / 1024).toFixed(1) + 'M' : k.toFixed(0) + 'K'; }

function renderServices(svcs) {
  if (!svcs || !svcs.length) { $('#services').innerHTML = '<span class="dim">—</span>'; return; }
  $('#services').innerHTML = svcs.map(s => {
    const dot = s.up ? 'ok' : 'bad';
    const inner = s.url ? `<a class="cmd" href="${esc(s.url)}" target="_blank" rel="noopener">${esc(s.name)}</a>` : `<span>${esc(s.name)}</span>`;
    return `<span><i class="g ${dot}"></i>${inner} <span class="dim">${esc(s.detail || '')}</span></span>`;
  }).join('');
}

// ---------- SSE wiring ----------
function connect() {
  const es = new EventSource('/events');
  es.addEventListener('hello', e => renderHello(JSON.parse(e.data)));
  es.addEventListener('stats', e => { renderStats(JSON.parse(e.data)); markFresh(); });
  es.addEventListener('devices', e => {
    tracked = JSON.parse(e.data) || [];
    const macs = new Set(tracked.map(d => d.mac));
    editing.forEach(m => { if (!macs.has(m)) editing.delete(m); });
    // Don't blow away an input the user is mid-edit in (draft row or a probe/port
    // field) or a text selection they're making — live updates resume the moment
    // they finish. The latest data is stored either way, so the next frame catches up.
    if (!editingInput() && !hasSelectionWithin('#tracked')) renderTracked();
  });
  es.addEventListener('cache', e => { cache = JSON.parse(e.data) || []; if (!hasSelectionWithin('#cache')) renderCache(); });
  es.addEventListener('scan', e => paintScan(JSON.parse(e.data)));
  es.addEventListener('wake', e => paintWake(JSON.parse(e.data)));
  es.addEventListener('services', e => renderServices(JSON.parse(e.data)));
}

// True while the user is actively typing in a TRACKED input (the add draft or an
// edit-mode field) — re-rendering then would discard their keystrokes.
function editingInput() {
  if (adding || editing.size > 0) {
    const a = document.activeElement;
    if (a && a.tagName === 'INPUT' && a.closest('#tracked')) return true;
  }
  return false;
}

// True while the user has a live (non-collapsed) text selection inside `sel`.
// The 1s re-render replaces innerHTML, which collapses any selection — so on a
// fast-refreshing table you could never highlight a MAC/IP to copy. We hold the
// re-render of that container until the selection is released.
function hasSelectionWithin(sel) {
  const s = window.getSelection();
  if (!s || s.isCollapsed || s.rangeCount === 0) return false;
  const root = document.querySelector(sel);
  if (!root) return false;
  const n = s.anchorNode;
  return !!(n && root.contains(n.nodeType === 1 ? n : n.parentNode));
}

// "data as at" freshness indicator: stamped when a stats frame arrives (so it
// reflects how current the data is, and incidentally reads as a clock while live).
// It freezes and goes amber if the stream stalls.
let lastStats = 0;
function markFresh() {
  lastStats = Date.now();
  $('#clock').textContent = new Date().toLocaleTimeString();
  $('#freshness').classList.remove('stale');
}

setDisc('cache');
renderTracked();
renderCache();
connect();
setInterval(() => { if (lastStats && Date.now() - lastStats > 3000) $('#freshness').classList.add('stale'); }, 1000);
