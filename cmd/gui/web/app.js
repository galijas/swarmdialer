// SwarmDialer frontend — plain vanilla JS, no build step. See
// docs/gui_spec.md for the spec this implements.

let wizardState = {
  server1: null, // {id, edition, ...} once provisioned
  server2: null,
  addingSecond: false,
};

// ---------- Tabs ----------

function showTab(name) {
  document.getElementById('tab-wizard').classList.toggle('hidden', name !== 'wizard');
  document.getElementById('tab-dashboard').classList.toggle('hidden', name !== 'dashboard');
  document.getElementById('tab-settings').classList.toggle('hidden', name !== 'settings');
  document.getElementById('tab-monitoring').classList.toggle('hidden', name !== 'monitoring');
  document.getElementById('tab-btn-monitoring').classList.toggle('active', name === 'monitoring');
  if (name === 'monitoring') startMonitoringPolling(); else stopMonitoringPolling();
  document.getElementById('tab-btn-wizard').classList.toggle('active', name === 'wizard');
  document.getElementById('tab-btn-dashboard').classList.toggle('active', name === 'dashboard');
  document.getElementById('tab-btn-settings').classList.toggle('active', name === 'settings');
  if (name === 'dashboard') {
    refreshServerList();
    connectStatusSockets();
    refreshLogList('local');
    refreshLogList('remote');
    startLogListPolling();
  }
  if (name === 'settings') {
    refreshSettingsTab();
  }
}

function showStep(id) {
  ['step-connect-1', 'step-provision-1', 'step-add-another', 'step-connect-2', 'step-provision-2', 'step-connect-trunk', 'step-serverware']
    .forEach(s => document.getElementById(s).classList.toggle('hidden', s !== id));
  if (id === 'step-serverware') prefillDTURL();
}

// ---------- API helpers ----------

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (res.status === 401) {
    // Session expired or logged out elsewhere: back to the login page.
    location.href = '/login.html';
    throw new Error('login required');
  }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

async function logout() {
  await fetch('/api/logout', {method: 'POST'}).catch(() => {});
  location.href = '/login.html';
}

function poll(fn, intervalMs, untilDone) {
  const tick = async () => {
    const result = await fn();
    if (untilDone(result)) return;
    setTimeout(tick, intervalMs);
  };
  tick();
}

// ---------- Wizard: connect ----------

async function testConnection(step) {
  const prefix = 'c' + step;
  const btn = document.getElementById(prefix + '-next-btn');
  if (btn.disabled) return; // guards against a double-click
  btn.disabled = true;
  const statusEl = document.getElementById(prefix + '-status');
  statusEl.textContent = 'Testing connection...';
  statusEl.className = 'status-line';
  try {
    const req = {
      base_url: document.getElementById(prefix + '-url').value.trim(),
      api_key: document.getElementById(prefix + '-key').value.trim(),
      api_key_v2: document.getElementById(prefix + '-key-v2').value.trim(),
    };
    const result = await api('/api/wizard/test-connection', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(req),
    });
    statusEl.textContent = `Connected (legacy + v2 keys both verified). Edition: ${result.edition}. License limits — Extensions: ${result.extensions}, Tenants: ${result.tenants}, DIDs: ${result.dids}, VOIP Trunks: ${result.voip_trunks}, Channels: ${result.channels}.`;

    const pIdx = step;
    const editionNote = document.getElementById('p' + pIdx + '-edition-note');
    const tenantFields = document.getElementById('p' + pIdx + '-tenant-fields');
    const isMultiTenant = result.edition.toLowerCase() === 'multi-tenant';
    tenantFields.classList.toggle('hidden', !isMultiTenant);
    editionNote.textContent = isMultiTenant
      ? 'Multi-Tenant edition detected — a tenant will be created.'
      : `"${result.edition}" edition detected — tenant creation is skipped; extensions are created at the system level. Channel limits and codecs are raised automatically via API v2.`;

    window['pending' + step] = req; // stash for provision() to reuse
    showStep('step-provision-' + step);
    // Left disabled — this step is done and showStep moved us past it;
    // there's no way back to re-click it.
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
    btn.disabled = false;
  }
}

// ---------- Wizard: provision ----------

async function provision(step) {
  const btn = document.getElementById('p' + step + '-create-btn');
  if (btn.disabled) return; // guards against a double-click firing two overlapping provisioning jobs
  btn.disabled = true;
  const pending = window['pending' + step];
  const statusEl = document.getElementById('p' + step + '-status');
  const barWrap = document.getElementById('p' + step + '-progress-bar');
  const bar = barWrap.querySelector('div');
  barWrap.classList.remove('hidden');
  statusEl.textContent = 'Starting...';
  statusEl.className = 'status-line';

  const req = {
    name: document.getElementById('c' + step + '-name').value.trim(),
    base_url: pending.base_url,
    api_key: pending.api_key,
    api_key_v2: pending.api_key_v2,
    extension_count: parseInt(document.getElementById('p' + step + '-ext-count').value, 10),
    tenant_code: document.getElementById('p' + step + '-tenant-code').value.trim(),
    tenant_name: document.getElementById('p' + step + '-tenant-name').value.trim(),
    // 4 digits (not 3) gives headroom for larger extension counts (and
    // therefore higher call volume, e.g. the +1000 dialer button) without
    // running out of numbers — matches what non-Multi-Tenant editions
    // already require anyway (see wizard.Provision's digit-length
    // detection for those).
    ext_length: 4, country: '869', national: '1', international: '011',
  };

  try {
    const {job_id} = await api('/api/wizard/provision', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(req),
    });

    poll(() => api('/api/wizard/provision/status?job_id=' + job_id), 2000, (p) => {
      const pct = p.total ? Math.round(100 * p.created / p.total) : 0;
      bar.style.width = pct + '%';
      statusEl.textContent = p.message || '';
      if (p.error) {
        statusEl.textContent = 'Error: ' + p.error;
        statusEl.className = 'status-line error';
        btn.disabled = false;
        return true;
      }
      if (p.done) {
        statusEl.textContent = `Done — ${p.created} extensions created.`;
        if (p.warning) addWizardNotice(p.warning);
        // Left disabled — this step is done and showStep moved us past it.
        if (step === 1) {
          wizardState.server1 = req;
          showStep('step-add-another');
        } else {
          wizardState.server2 = req;
          showStep('step-connect-trunk');
        }
        return true;
      }
      return false;
    });
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
    btn.disabled = false;
  }
}

// addWizardNotice shows a non-fatal provisioning warning in a box at the
// top of the wizard, which stays visible through the remaining steps
// (showStep swaps panels, so a step's own status line would disappear).
function addWizardNotice(text) {
  const box = document.getElementById('wizard-notices');
  const p = document.createElement('p');
  p.textContent = '⚠ ' + text;
  box.appendChild(p);
  box.classList.remove('hidden');
}

// ---------- Wizard: SERVERware ----------

async function connectServerware() {
  const btn = document.getElementById('sw-connect-btn');
  if (btn.disabled) return;
  btn.disabled = true;
  const skip = document.getElementById('sw-skip-btn');
  skip.disabled = true;
  const statusEl = document.getElementById('sw-status');
  const stepsEl = document.getElementById('sw-steps');
  stepsEl.innerHTML = '';
  statusEl.textContent = 'Starting...';
  statusEl.className = 'status-line';

  const req = {
    controller: document.getElementById('sw-controller').value.trim(),
    api_key: document.getElementById('sw-key').value.trim(),
    dt_collector_url: document.getElementById('sw-dt-url').value.trim(),
    dt_collector_key: document.getElementById('sw-dt-key').value.trim(),
  };
  const retry = (msg) => {
    statusEl.textContent = 'Error: ' + msg;
    statusEl.className = 'status-line error';
    btn.disabled = false;
    skip.disabled = false;
  };
  try {
    const {job_id} = await api('/api/wizard/serverware', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(req),
    });
    poll(() => api('/api/wizard/serverware/status?job_id=' + job_id), 2000, (p) => {
      stepsEl.innerHTML = '';
      (p.steps || []).forEach(t => {
        const li = document.createElement('li');
        li.textContent = '✓ ' + t;
        stepsEl.appendChild(li);
      });
      (p.warnings || []).forEach(t => {
        const li = document.createElement('li');
        li.className = 'warn';
        li.textContent = '⚠ ' + t;
        stepsEl.appendChild(li);
      });
      statusEl.textContent = p.message ? p.message + '...' : '';
      if (p.error) { retry(p.error); return true; }
      if (p.done) {
        statusEl.textContent = 'SERVERware connected.';
        document.getElementById('sw-complete-btn').classList.remove('hidden');
        return true;
      }
      return false;
    });
  } catch (e) {
    retry(e.message);
  }
}

async function prefillDTURL() {
  const el = document.getElementById('sw-dt-url');
  if (el.value) return;
  try { el.value = (await api('/api/dtcollector')).url; } catch (e) { /* placeholder stays */ }
}

function startSecondServer() {
  wizardState.addingSecond = true;
  showStep('step-connect-2');
}

async function finishWizard() {
  try {
    await api('/api/wizard/complete', {method: 'POST'});
    setWizardVisible(false);
  } catch (e) { /* the tab just stays; the next visit re-checks */ }
  showTab('dashboard');
}

// ---------- Wizard: connect trunk + DIDs ----------

async function connectServers() {
  const btn = document.getElementById('ct-create-btn');
  if (btn.disabled) return; // guards against a double-click firing two overlapping connect jobs
  btn.disabled = true;

  const statusEl = document.getElementById('ct-status');
  const barWrap = document.getElementById('ct-progress-bar');
  const bar = barWrap.querySelector('div');
  barWrap.classList.remove('hidden');
  statusEl.textContent = 'Fetching server list...';

  try {
    const {servers} = await api('/api/servers');
    if (servers.length < 2) {
      statusEl.textContent = 'Error: need two provisioned servers first.';
      statusEl.className = 'status-line error';
      btn.disabled = false;
      return;
    }
    const server1 = servers[servers.length - 2];
    const server2 = servers[servers.length - 1];

    const {job_id} = await api('/api/wizard/connect', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({server1_id: server1.id, server2_id: server2.id}),
    });

    poll(() => api('/api/wizard/connect/status?job_id=' + job_id), 2000, (p) => {
      const pct = p.total ? Math.round(100 * p.created / p.total) : (p.stage === 'settling' ? 90 : 10);
      bar.style.width = pct + '%';
      statusEl.textContent = `Stage: ${p.stage || '...'}` + (p.total ? ` (${p.created}/${p.total})` : '');
      if (p.error) {
        statusEl.textContent = 'Error: ' + p.error;
        statusEl.className = 'status-line error';
        btn.disabled = false;
        return true;
      }
      if (p.done) {
        statusEl.textContent = 'Trunk and DIDs created.';
        // Left disabled — this step is done; "Complete Setup" takes over.
        document.getElementById('ct-complete-btn').classList.remove('hidden');
        return true;
      }
      return false;
    });
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
    btn.disabled = false;
  }
}

// ---------- Dashboard: server list ----------

async function refreshServerList() {
  const {servers, wizard_completed} = await api('/api/servers');
  setWizardVisible(!wizard_completed);
  document.getElementById('servers-count').textContent = servers.length;
  const list = document.getElementById('server-list');
  list.innerHTML = '';
  servers.forEach(s => {
    const div = document.createElement('div');
    div.className = 'server-card';
    div.innerHTML = `
      <div>
        <div class="name">${s.name} <span class="badge">${s.edition}</span></div>
        <div class="meta">${s.base_url} — tenant ${s.tenant_code || '(system)'} — ${s.extension_count} extensions${s.did_count ? ' — ' + s.did_count + ' DIDs' : ''}</div>
      </div>`;
    list.appendChild(div);
  });

  // Which server "local" dials from, and which connected pair "remote"
  // dials between, are explicit choices — with more than one server
  // configured there's no single implicit answer (see the dashboard's
  // server/pair selects). Both directions of a connected pair are listed
  // separately since the caller side matters (whose extensions initiate).
  populateSelect('local-server-select', servers.map(s => ({value: s.id, label: s.name})));

  const byID = new Map(servers.map(s => [s.id, s]));
  const pairs = [];
  servers.forEach(s => {
    const peer = s.peer_server_id && byID.get(s.peer_server_id);
    if (peer) pairs.push({value: `${s.id}:${peer.id}`, label: `${s.name} → ${peer.name}`});
  });
  populateSelect('remote-pair-select', pairs);

  document.getElementById('remote-dialer-panel').classList.toggle('disabled-overlay', pairs.length === 0);

  refreshRecordingStatus('local');
  refreshRecordingStatus('remote');
}

function populateSelect(id, options) {
  const select = document.getElementById(id);
  const prevValue = select.value;
  select.innerHTML = '';
  options.forEach(opt => {
    const el = document.createElement('option');
    el.value = opt.value;
    el.textContent = opt.label;
    select.appendChild(el);
  });
  if (options.some(opt => opt.value === prevValue)) select.value = prevValue;
}

// ---------- Dashboard: dialing ----------

// Resolves which server (and, for remote, which peer) a dialer section is
// currently pointed at — shared by confirmDial and confirmStop so they
// always agree on which session a click applies to.
function dialerTarget(section) {
  if (section === 'remote') {
    const pairValue = document.getElementById('remote-pair-select').value;
    if (!pairValue) { logLine('Error: no connected server pair selected.', true); return null; }
    const [serverID, peerServerID] = pairValue.split(':');
    return {serverID, peerServerID};
  }
  const serverID = document.getElementById('local-server-select').value;
  if (!serverID) { logLine('Error: no server selected.', true); return null; }
  return {serverID, peerServerID: undefined};
}

function confirmDial(section, count) {
  if (!confirm(`Start ${count} additional ${section} call(s)?`)) return;
  const duration = parseInt(document.getElementById(section + '-duration').value, 10);
  const useRTP = document.getElementById(section + '-rtp').checked;
  const codec = document.getElementById(section + '-codec').value;

  const target = dialerTarget(section);
  if (!target) return;
  const {serverID, peerServerID} = target;

  api('/api/dial', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      section, server_id: serverID, peer_server_id: peerServerID,
      count, call_duration_seconds: duration, use_rtp: useRTP, codec,
    }),
  }).then(r => {
    if (r.trunk_note) logLine(r.trunk_note);
    logLine(`Requested +${count} ${section} calls (${codec}) — ${r.started} started (pool availability may limit this). A log will appear in ${section === 'local' ? 'Local' : 'Remote'} Call Logs once the batch finishes.`);
  }).catch(e => logLine('Error: ' + e.message, true));
}

function confirmStop(section) {
  const target = dialerTarget(section);
  if (!target) return;
  if (!confirm(`Stop ALL ${section} calls? This hangs up everything currently active and cancels anything still queued.`)) return;
  const {serverID, peerServerID} = target;

  api('/api/stop', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({section, server_id: serverID, peer_server_id: peerServerID}),
  }).then(r => {
    logLine(r.stopped ? `Stopped all ${section} calls.` : `No active ${section} session to stop.`);
  }).catch(e => logLine('Error: ' + e.message, true));
}

// ---------- Dashboard: recording ----------
//
// Recording is a property of the target PBXware instance (via API v2), not
// of SwarmDialer's own session state, so its controls always reflect
// whatever the server/pair selection currently resolves to — refreshed on
// selection change and right after every toggle/change, rather than
// tracked as separate local UI state that could drift from what PBXware
// actually has set.

// Same target resolution as dialerTarget, but silent on "nothing selected"
// (returns null) instead of logging to Live Status — refreshRecordingStatus
// runs on every server-list refresh, including the ordinary case of a
// single server with no remote pair yet, and that isn't an error worth a
// log line.
function recordingDialerTarget(section) {
  if (section === 'remote') {
    const pairValue = document.getElementById('remote-pair-select').value;
    if (!pairValue) return null;
    const [serverID, peerServerID] = pairValue.split(':');
    return {serverID, peerServerID};
  }
  const serverID = document.getElementById('local-server-select').value;
  if (!serverID) return null;
  return {serverID, peerServerID: undefined};
}

function applyRecordingState(section, st) {
  const toggleBtn = document.getElementById(section + '-recording-toggle');
  const stereoCb = document.getElementById(section + '-recording-stereo');
  const formatSel = document.getElementById(section + '-recording-format');
  toggleBtn.textContent = st.enabled ? 'Disable Recording' : 'Enable Recording';
  toggleBtn.classList.toggle('recording-on', st.enabled);
  stereoCb.checked = st.stereo_enabled;
  stereoCb.disabled = !st.enabled;
  formatSel.disabled = !st.enabled;
  if (st.format) formatSel.value = st.format;
  applyCodecRecordingRestriction(section, st.enabled);
}

// PBXware has no G.729 transcoder (only passthrough), and recording has to
// decode the audio, so G.729 calls are silently never recorded — confirmed
// live 2026-09-25 ("No Translation Path" g729 -> slin in Asterisk). While
// recording is on, G.729 is greyed out; if it was selected, the dialer
// falls back to G.711 so the next batch actually gets recorded.
function applyCodecRecordingRestriction(section, recordingOn) {
  const codecSel = document.getElementById(section + '-codec');
  const g729 = codecSel.querySelector('option[value="g729"]');
  g729.disabled = recordingOn;
  g729.textContent = recordingOn ? 'G.729 (not recordable)' : 'G.729';
  if (recordingOn && codecSel.value === 'g729') {
    codecSel.value = 'ulaw';
    logLine(`${section === 'local' ? 'Local' : 'Remote'} dialer: switched codec from G.729 to G.711 (ulaw), since PBXware can't record G.729 calls.`);
  }
}

async function refreshRecordingStatus(section) {
  const toggleBtn = document.getElementById(section + '-recording-toggle');
  const stereoCb = document.getElementById(section + '-recording-stereo');
  const formatSel = document.getElementById(section + '-recording-format');
  const statusEl = document.getElementById(section + '-recording-status');
  const target = recordingDialerTarget(section);
  if (!target) {
    toggleBtn.disabled = true;
    stereoCb.disabled = true;
    formatSel.disabled = true;
    statusEl.textContent = '';
    applyCodecRecordingRestriction(section, false);
    return;
  }
  toggleBtn.disabled = false;
  try {
    const st = await api(`/api/recording/status?section=${section}&server_id=${encodeURIComponent(target.serverID)}&peer_server_id=${encodeURIComponent(target.peerServerID || '')}`);
    applyRecordingState(section, st);
    statusEl.textContent = '';
  } catch (e) {
    statusEl.textContent = 'Error loading recording status: ' + e.message;
    statusEl.className = 'status-line error';
  }
}

async function setRecordingState(section, target, enabled, stereo, format, statusEl) {
  statusEl.textContent = 'Updating...';
  statusEl.className = 'status-line';
  try {
    const st = await api('/api/recording/toggle', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        section, server_id: target.serverID, peer_server_id: target.peerServerID,
        enabled, stereo, format,
      }),
    });
    applyRecordingState(section, st);
    statusEl.textContent = '';
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
}

function toggleRecording(section) {
  const target = recordingDialerTarget(section);
  if (!target) return;
  const toggleBtn = document.getElementById(section + '-recording-toggle');
  const stereoCb = document.getElementById(section + '-recording-stereo');
  const formatSel = document.getElementById(section + '-recording-format');
  const statusEl = document.getElementById(section + '-recording-status');
  const enabling = toggleBtn.textContent === 'Enable Recording';
  setRecordingState(section, target, enabling, enabling && stereoCb.checked, formatSel.value, statusEl);
}

// Fires when the stereo checkbox or format dropdown changes — both are
// only enabled while recording itself is already on, so this always sends
// enabled:true alongside whatever changed.
function onRecordingChange(section) {
  const target = recordingDialerTarget(section);
  if (!target) return;
  const stereoCb = document.getElementById(section + '-recording-stereo');
  const formatSel = document.getElementById(section + '-recording-format');
  const statusEl = document.getElementById(section + '-recording-status');
  setRecordingState(section, target, true, stereoCb.checked, formatSel.value, statusEl);
}

// ---------- Dashboard: live status ----------
//
// One socket carries every live session at once (a session per server for
// local dialing, a session per connected pair for remote — see
// cmd/gui/app.go's allSnapshots), each tagged with a stable key, a type
// ("local"/"remote" — colors the graph/log), and a label (the server or
// pair name — shown in the log tag). Everything is merged into one
// combined log/graph/stats display rather than switched between, so you
// see the whole system at once instead of only ever half of it.

let ws = null;
let wsReconnectTimer = null;
const lastSeenEventSeq = new Map(); // session key -> last-shown event seq
const graphRows = new Map(); // `${sessionKey}:${callId}` -> row element

function connectStatusSockets() {
  if (wsReconnectTimer) { clearTimeout(wsReconnectTimer); wsReconnectTimer = null; }
  if (ws) { ws.onclose = null; ws.close(); }
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const sock = new WebSocket(`${proto}://${location.host}/ws/status`);
  sock.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    const sessions = msg.sessions || [];
    renderRegistrations(msg.registrations || []);
    renderCombinedStats(sessions);
    renderEvents(sessions);
    renderGraph(sessions);
  };
  // Without this, a dropped connection (GUI server restart, network blip)
  // leaves the dashboard silently frozen — stats/log/graph all stop
  // updating with no visible sign anything is wrong, since the one-off
  // "N started" log line comes from the /api/dial response, not the socket.
  sock.onclose = () => {
    wsReconnectTimer = setTimeout(connectStatusSockets, 2000);
  };
  ws = sock;
}

function renderCombinedStats(sessions) {
  const sum = (key) => sessions.reduce((total, s) => total + (s.snapshot[key] || 0), 0);
  const activeCount = sessions.reduce((total, s) => total + (s.snapshot.active_calls || []).length, 0);
  document.getElementById('stat-active').textContent = activeCount;
  document.getElementById('stat-started').textContent = sum('total_calls_started');
  document.getElementById('stat-answered').textContent = sum('total_calls_answered');
  document.getElementById('stat-failed').textContent = sum('total_calls_failed');
  document.getElementById('stat-rtp').textContent = sum('total_rtp_sent') + sum('total_rtp_recv');
}

// Log view: one line per lifecycle event (dialing/answered/failed/ended/
// batch_done), tagged with the session's label — recent_events is a
// server-side ring buffer per session, we only log the ones newer than
// the last one we've already shown for that specific session.
function renderEvents(sessions) {
  sessions.forEach(s => {
    (s.snapshot.recent_events || []).forEach(ev => {
      const lastSeen = lastSeenEventSeq.get(s.key) || 0;
      if (ev.seq <= lastSeen) return;
      lastSeenEventSeq.set(s.key, ev.seq);
      const tag = `<span class="tag ${s.type}">${s.label}</span>`;
      const pair = `${ev.caller_aor} &rarr; ${ev.callee_aor}`;
      if (ev.type === 'dialing') logLine(`${tag}dialing ${pair}`);
      else if (ev.type === 'answered') logLine(`${tag}<span class="ok">answered</span> ${pair}`);
      else if (ev.type === 'failed') logLine(`${tag}<span class="err">failed</span> ${pair} — ${ev.detail}`, true);
      else if (ev.type === 'ended') logLine(`${tag}ended ${pair} (${ev.detail})`);
      else if (ev.type === 'batch_done') logLine(`${tag}<strong>${ev.detail}</strong>`);
    });
  });
}

// Registration progress: a session's first dial registers every one of
// its extensions before any call starts (~20s+ for 1000), which used to
// happen with no sign of life in the dashboard. Each registration attempt
// gets one log line, updated in place until it finishes.
const registrationRows = new Map(); // registration id -> {row, done}

function registrationText(r) {
  const fmtSide = (sd) => `${sd.registered}/${sd.total}` + (sd.failed ? ` (${sd.failed} failed)` : '');
  const sides = Object.entries(r.sides);
  if (sides.length === 0) return '0 extensions so far';
  if (sides.length === 1 && sides[0][0] === '') return fmtSide(sides[0][1]);
  return sides.map(([side, sd]) => `${side} ${fmtSide(sd)}`).join(', ');
}

function renderRegistrations(regs) {
  regs.forEach(r => {
    let entry = registrationRows.get(r.id);
    if (entry && entry.done) return;
    const tag = `<span class="tag ${r.type}">${r.label}</span>`;
    let html, isError = false;
    if (r.message) html = `${tag}${r.done ? (r.done_message || r.message) : r.message}`;
    else if (!r.done) html = `${tag}registering extensions: ${registrationText(r)}`;
    else if (r.error) { html = `${tag}<span class="err">registration failed</span> (${registrationText(r)}): ${r.error}`; isError = true; }
    else html = `${tag}<span class="ok">registered</span> ${registrationText(r)} extensions, starting calls`;
    if (!entry) {
      entry = {row: logLine(html, isError), done: false};
      registrationRows.set(r.id, entry);
    } else {
      entry.row.innerHTML = `[${new Date().toLocaleTimeString()}] ${html}`;
      entry.row.classList.toggle('error', isError);
    }
    entry.done = r.done;
  });
}

// Call graph: one row per active call — two dots joined by an animated
// dashed line (a little "data flowing" motion), colored by whether it's a
// local or remote call. Rows are created/removed as calls start/end and
// otherwise updated in place, so the flow animation never restarts.
function renderGraph(sessions) {
  const seen = new Set();
  sessions.forEach(s => {
    (s.snapshot.active_calls || []).forEach(c => {
      const rowKey = `${s.key}:${c.id}`;
      seen.add(rowKey);
      let row = graphRows.get(rowKey);
      if (!row) {
        row = document.createElement('div');
        row.className = `call-graph-row ${s.type}`;
        row.innerHTML = `<span class="tag ${s.type}">${s.label}</span>` +
          '<span class="call-dot"></span><span class="call-line"></span>' +
          '<span class="call-dot"></span><span class="call-label"></span>';
        document.getElementById('call-graph-rows').appendChild(row);
        graphRows.set(rowKey, row);
      }
      row.classList.toggle('rtp-off', !(c.rtp_sent > 0 || c.rtp_recv > 0));
      row.querySelector('.call-label').textContent = `${c.caller_aor} → ${c.callee_aor}`;
    });
  });
  for (const [key, row] of graphRows) {
    if (!seen.has(key)) {
      row.remove();
      graphRows.delete(key);
    }
  }
}

function setStatusView(view) {
  document.getElementById('view-btn-log').classList.toggle('active', view === 'log');
  document.getElementById('view-btn-graph').classList.toggle('active', view === 'graph');
  document.getElementById('call-log').classList.toggle('hidden', view !== 'log');
  document.getElementById('call-graph').classList.toggle('hidden', view !== 'graph');
}

function clearLog() {
  document.getElementById('call-log').innerHTML = '';
}

function logLine(html, isError) {
  const log = document.getElementById('call-log');
  const row = document.createElement('div');
  row.className = 'row' + (isError ? ' error' : '');
  const time = new Date().toLocaleTimeString();
  row.innerHTML = `[${time}] ${html}`;
  log.appendChild(row);
  log.scrollTop = log.scrollHeight;
  return row;
}

// ---------- Dashboard: log browser ----------

function formatBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

// Keeps the log list/count current while the dashboard is open, instead of
// relying on a one-shot timer guessing when a batch's log will be ready
// (confirmDial previously scheduled a refresh at duration+15s — too early
// for a batch whose actual completion, after mosFinalizeDelay and CDR/MOS
// lookups, ran longer than that guess, leaving the list stuck until the
// next unrelated trigger like a tab switch). Runs for the life of the
// page once started — same as the live-status WebSocket, cheap enough
// not to bother stopping on tab switch.
let logListPollTimer = null;
function startLogListPolling() {
  if (logListPollTimer) return;
  logListPollTimer = setInterval(() => {
    refreshLogList('local');
    refreshLogList('remote');
  }, 5000);
}

function toggleLogPanel(section) {
  document.getElementById(section + '-log-list').classList.toggle('collapsed');
  document.getElementById(section + '-log-header').classList.toggle('expanded');
}

async function refreshLogList(section) {
  const list = document.getElementById(section + '-log-list');
  const countEl = document.getElementById(section + '-log-count');
  try {
    const {logs} = await api('/api/logs?section=' + section);
    if (countEl) countEl.textContent = logs ? logs.length : 0;
    if (!logs || logs.length === 0) {
      list.innerHTML = '<p class="status-line">No logs yet — run a batch of calls to create one.</p>';
      return;
    }
    list.innerHTML = '';
    logs.forEach(l => {
      const row = document.createElement('div');
      row.className = 'log-row';
      const when = new Date(l.mod_time).toLocaleString();
      row.innerHTML = `
        <div class="log-row-info">
          <div class="log-row-name">${l.name}</div>
          <div class="log-row-meta">${when} — ${formatBytes(l.size_bytes)}</div>
        </div>
        <div class="log-row-actions">
          <button class="btn secondary" onclick="viewLog('${section}','${l.name}')">View</button>
          <button class="btn secondary" onclick="downloadLog('${section}','${l.name}')">Download</button>
          <button class="btn danger" onclick="deleteLog('${section}','${l.name}')">Delete</button>
        </div>`;
      list.appendChild(row);
    });
  } catch (e) {
    list.innerHTML = `<p class="status-line error">Error loading logs: ${e.message}</p>`;
  }
}

function viewLog(section, name) {
  window.open(`/api/logs/view?section=${encodeURIComponent(section)}&name=${encodeURIComponent(name)}`, '_blank');
}

function downloadLog(section, name) {
  window.open(`/api/logs/download?section=${encodeURIComponent(section)}&name=${encodeURIComponent(name)}`, '_blank');
}

async function deleteLog(section, name) {
  if (!confirm(`Delete log "${name}"? This cannot be undone.`)) return;
  try {
    await api('/api/logs/delete', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({section, name}),
    });
    refreshLogList(section);
  } catch (e) {
    logLine('Error deleting log: ' + e.message, true);
  }
}

// ---------- Settings: reset ----------

async function refreshServerwareSettings() {
  const el = document.getElementById('sw-settings-status');
  const btn = document.getElementById('sw-settings-btn');
  try {
    const sw = await api('/api/serverware');
    if (!sw.configured) {
      el.textContent = 'Not connected. Connect the SERVERware site to use host monitoring and the test script.';
      btn.textContent = 'Connect SERVERware';
      return;
    }
    el.textContent = `Connected to ${sw.controller_url}, host ${sw.host_name} (VPSs: ${sw.vps_names.join(', ')}). ` +
      (sw.dt_collector_configured ? `Reports upload to ${sw.dt_collector_url}.` : 'DT Collector not set yet.');
    btn.textContent = 'Reconnect SERVERware';
  } catch (e) {
    el.textContent = 'Error loading SERVERware status: ' + e.message;
  }
}

async function refreshDTCollectorSettings() {
  try {
    const dt = await api('/api/dtcollector');
    document.getElementById('dt-url').value = dt.url;
    const key = document.getElementById('dt-key');
    key.value = '';
    key.placeholder = dt.key_set ? 'saved (leave empty to keep it)' : 'not set';
  } catch (e) {
    document.getElementById('dt-status').textContent = 'Error: ' + e.message;
  }
}

async function saveDTCollector() {
  const btn = document.getElementById('dt-save-btn');
  const statusEl = document.getElementById('dt-status');
  btn.disabled = true;
  statusEl.textContent = 'Checking the key with DT Collector...';
  statusEl.className = 'status-line';
  try {
    await api('/api/dtcollector', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url: document.getElementById('dt-url').value.trim(), key: document.getElementById('dt-key').value.trim()}),
    });
    statusEl.textContent = 'Saved. DT Collector accepted the upload key.';
    refreshDTCollectorSettings();
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  btn.disabled = false;
}

async function refreshSettingsTab() {
  refreshInstances();
  refreshServerwareSettings();
  refreshDTCollectorSettings();
  const {servers} = await api('/api/servers');
  [0, 1].forEach(i => {
    const btn = document.getElementById(`reset-instance-${i + 1}-btn`);
    const srv = servers[i];
    if (srv) {
      btn.disabled = false;
      btn.textContent = `Reset ${srv.name}`;
      btn.dataset.serverId = srv.id;
    } else {
      btn.disabled = true;
      btn.textContent = 'Reset —';
      delete btn.dataset.serverId;
    }
  });
}

// Disables (or re-enables) every reset-related button together, so an
// instance reset, "reset SwarmDialer," and "reset all" can never overlap
// — running two of these concurrently against the same server raced in
// practice (one job's trunk/tenant delete stepping on the other's, the
// second's store removal then failing with "no server with id ...").
function setSettingsResetButtonsDisabled(disabled) {
  ['reset-instance-1-btn', 'reset-instance-2-btn', 'reset-swarmdialer-btn', 'reset-all-btn', 'clear-logs-btn'].forEach(id => {
    document.getElementById(id).disabled = disabled;
  });
}

async function clearAllLogs() {
  const btn = document.getElementById('clear-logs-btn');
  if (btn.disabled) return;
  if (!confirm('Delete every call log, local and remote? This cannot be undone.')) return;

  setSettingsResetButtonsDisabled(true);
  const statusEl = document.getElementById('clear-logs-status');
  statusEl.textContent = 'Clearing...';
  statusEl.className = 'status-line';
  try {
    await api('/api/settings/clear-logs', {method: 'POST'});
    statusEl.textContent = 'All logs cleared.';
    refreshLogList('local');
    refreshLogList('remote');
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  setSettingsResetButtonsDisabled(false);
}

// Starts a reset-instance job and polls it to completion, updating
// statusEl live with whatever step is currently running (e.g. "deleting
// tenant 18", "waiting for tenant 18 to finish deleting (can take
// several minutes)") — a Multi-Tenant reset can take several minutes, so
// showing only a static "resetting..." message the whole time leaves the
// user with no way to tell it's still working versus stuck. label, if
// given, prefixes every status line (used by resetAll to show which
// instance is currently being reset).
function runResetInstanceJob(serverID, statusEl, label) {
  const prefix = label ? `${label}: ` : '';
  return api('/api/settings/reset-instance', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({server_id: serverID}),
  }).then(({job_id}) => new Promise((resolve) => {
    poll(() => api('/api/settings/reset-instance/status?job_id=' + job_id), 1500, (p) => {
      if (!p.done) {
        statusEl.textContent = prefix + (p.message || 'working...');
        statusEl.className = 'status-line';
        return false;
      }
      statusEl.textContent = p.warning ? `${prefix}done, with warnings: ${p.warning}` : `${prefix}done.`;
      statusEl.className = p.warning ? 'status-line error' : 'status-line';
      resolve(p);
      return true;
    });
  }));
}

async function resetInstance(index) {
  const btn = document.getElementById(`reset-instance-${index + 1}-btn`);
  const serverID = btn.dataset.serverId;
  if (!serverID || btn.disabled) return; // guards against a double-click firing two overlapping reset jobs
  const name = btn.textContent.replace('Reset ', '');
  if (!confirm(`Reset ${name}? This deletes everything SwarmDialer created on that PBXware instance (trunk, tenant/package or extensions) and cannot be undone.`)) return;

  setSettingsResetButtonsDisabled(true);
  const statusEl = document.getElementById('reset-instance-status');
  statusEl.textContent = 'Starting...';
  statusEl.className = 'status-line';
  try {
    await runResetInstanceJob(serverID, statusEl, name);
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  setSettingsResetButtonsDisabled(false);
  refreshSettingsTab(); // re-disables the instance buttons if their server is now gone
}

async function resetSwarmDialer() {
  const btn = document.getElementById('reset-swarmdialer-btn');
  if (btn.disabled) return; // guards against a double-click
  if (!confirm('Reset SwarmDialer to default? This deletes SwarmDialer\'s saved configuration and restarts the GUI. It does NOT touch anything on PBXware.')) return;

  setSettingsResetButtonsDisabled(true);
  const statusEl = document.getElementById('reset-swarmdialer-status');
  statusEl.textContent = 'Resetting and restarting...';
  statusEl.className = 'status-line';
  try {
    await api('/api/settings/reset-swarmdialer', {method: 'POST'});
  } catch (e) {
    // A dropped connection here is expected — the process restarts right
    // after responding.
  }
  await waitForRestart(statusEl); // page reloads once back up, so no need to re-enable btn here
}

async function resetAll() {
  const btn = document.getElementById('reset-all-btn');
  if (btn.disabled) return; // guards against a double-click
  if (!confirm('Reset ALL connected instances and SwarmDialer itself? This cannot be undone.')) return;

  setSettingsResetButtonsDisabled(true);
  const statusEl = document.getElementById('reset-all-status');
  statusEl.className = 'status-line';
  try {
    const {servers} = await api('/api/servers');
    for (const s of servers) {
      await runResetInstanceJob(s.id, statusEl, s.name);
    }
    statusEl.textContent = 'Resetting SwarmDialer...';
    await api('/api/settings/reset-swarmdialer', {method: 'POST'});
  } catch (e) {
    // ignore — a dropped connection is expected once the process restarts
  }
  await waitForRestart(statusEl); // page reloads once back up, so no need to re-enable btn here
}

// Polls /api/servers until the just-restarted process answers again (it
// briefly refuses connections while the exec-restart is in flight), then
// reloads so the UI reflects the now-empty config from a clean slate.
async function waitForRestart(statusEl) {
  for (let i = 0; i < 30; i++) {
    await new Promise(r => setTimeout(r, 1000));
    try {
      await api('/api/servers');
      statusEl.textContent = 'Done — SwarmDialer restarted.';
      setTimeout(() => location.reload(), 500);
      return;
    } catch (e) {
      // still restarting
    }
  }
  statusEl.textContent = 'Restart is taking longer than expected — refresh the page manually.';
  statusEl.className = 'status-line error';
}

// ---------- Wizard tab visibility ----------
//
// The Setup Wizard is only useful until it's finished: after that its tab
// is hidden, and instances are reconfigured from Settings. It comes back
// when no instance is left (e.g. after resetting them all).

function setWizardVisible(visible) {
  document.getElementById('tab-btn-wizard').classList.toggle('hidden', !visible);
}

(async function init() {
  try {
    const {servers, wizard_completed} = await api('/api/servers');
    setWizardVisible(!wizard_completed);
    if (wizard_completed || servers.length > 0) showTab('dashboard');
  } catch (e) {
    // Can't reach the API: stay on the wizard (the default) rather than
    // silently failing somewhere less obvious.
  }
})();

// ---------- Collapsible sections ----------

function toggleSection(name) {
  document.getElementById(name + '-body').classList.toggle('collapsed');
  document.getElementById(name + '-header').classList.toggle('expanded');
}

function setSectionOpen(name, open) {
  document.getElementById(name + '-body').classList.toggle('collapsed', !open);
  document.getElementById(name + '-header').classList.toggle('expanded', open);
}

// ---------- Settings: PBXware instances ----------

async function refreshInstances() {
  const list = document.getElementById('inst-list');
  try {
    const {servers} = await api('/api/servers');
    list.innerHTML = servers.map(s => `
      <div class="inst-card" data-id="${esc(s.id)}">
        <div class="inst-head"><span class="name">${esc(s.name)}</span> <span class="badge">${esc(s.edition)}</span>
          <span class="meta">tenant ${esc(s.tenant_code || '(system)')} · ${s.extension_count} extensions${s.did_count ? ' · ' + s.did_count + ' DIDs' : ''}${s.peer_server_id ? ' · trunk connected' : ''}</span></div>
        <div class="form-grid">
          <div class="form-row"><label>Name</label><input class="inst-name" value="${esc(s.name)}"></div>
          <div class="form-row"><label>Base URL</label><input class="inst-url" value="${esc(s.base_url)}"></div>
          <div class="form-row"><label>Legacy API key</label><input class="inst-key" type="password" autocomplete="off" placeholder="saved (leave empty to keep it)"></div>
          <div class="form-row"><label>API v2 key</label><input class="inst-key2" type="password" autocomplete="off" placeholder="saved (leave empty to keep it)"></div>
        </div>
        <button class="btn secondary" onclick="saveInstance(this)">Save</button>
        <span class="status-line inst-status"></span>
      </div>`).join('') || '<p class="status-line">No instances connected yet. Run the Setup Wizard.</p>';
    document.getElementById('inst-add-btn').classList.toggle('hidden', servers.length !== 1);
  } catch (e) {
    list.innerHTML = `<p class="status-line error">Error loading instances: ${esc(e.message)}</p>`;
  }
}

async function saveInstance(btn) {
  const card = btn.closest('.inst-card');
  const statusEl = card.querySelector('.inst-status');
  btn.disabled = true;
  statusEl.textContent = 'Testing the connection...';
  statusEl.className = 'status-line inst-status';
  try {
    await api('/api/servers/update', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        id: card.dataset.id,
        name: card.querySelector('.inst-name').value.trim(),
        base_url: card.querySelector('.inst-url').value.trim(),
        api_key: card.querySelector('.inst-key').value.trim(),
        api_key_v2: card.querySelector('.inst-key2').value.trim(),
      }),
    });
    statusEl.textContent = 'Saved.';
    refreshInstances();
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error inst-status';
  }
  btn.disabled = false;
}

async function addSecondInstance() {
  try {
    await api('/api/wizard/complete', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({completed: false})});
  } catch (e) { /* the wizard still opens */ }
  setWizardVisible(true);
  showTab('wizard');
  startSecondServer();
}

// ---------- SW Host Benchmark ----------

let monProfiles = [];
let monTimer = null;
let lastRunStatus = null;

const PHASES = {baseline: 'Idle baseline', ramp: 'Ramping up', hold: 'Holding at target', rolling: 'Rolling calls', stopping: 'Stopping calls', cooldown: 'Cooldown'};
const PHASE_ORDER = {ramp: ['baseline', 'ramp', 'hold', 'stopping', 'cooldown'], rolling: ['baseline', 'rolling', 'stopping', 'cooldown']};
const STOP_REASONS = {
  target_reached: 'Target reached', target_not_reached: 'Target not reached', host_cpu_100: 'Host CPU saturated',
  host_ram_100: 'Host memory full', vps_cpu_limit: 'VPS CPU limit reached', vps_ram_limit: 'VPS memory limit reached',
  swarmdialer_overloaded: 'SwarmDialer overloaded (result invalid)', cancelled: 'Cancelled', error: 'Error',
};
// Validated categorical palette on the dark chart surface (dataviz check:
// all pass). Fixed order, never cycled: host, PBXware instance 1, instance 2.
const SERIES = [
  {key: 'host', label: 'SW host', color: ''},
  {key: 'MT', label: 'PBXware MT VPS', color: ''},
  {key: 'CC', label: 'PBXware CC VPS', color: ''},
];
// Series colours come from the theme (--series-1..3 in style.css), so
// light and dark mode each use their own validated palette.
const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
function applySeriesColors() { SERIES.forEach((s, i) => { s.color = cssVar(`--series-${i + 1}`); }); }
document.addEventListener('themechange', () => {
  if (!document.getElementById('tab-monitoring').classList.contains('hidden')) renderShowcase();
});
function esc(v) { return String(v ?? '').replace(/[&<>"]/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'}[c])); }
const fmt1 = (v) => (v === undefined || v === null || isNaN(v)) ? '–' : (Math.round(v * 10) / 10).toString();

function startMonitoringPolling() {
  refreshMonitoring(true);
  if (!monTimer) monTimer = setInterval(() => refreshMonitoring(false), 3000);
}
function stopMonitoringPolling() {
  if (monTimer) { clearInterval(monTimer); monTimer = null; }
}

async function refreshMonitoring(full) {
  try {
    if (full || monProfiles.length === 0) {
      monProfiles = await api('/api/testrun/profiles');
      const sel = document.getElementById('mon-profile');
      const keep = sel.value;
      // Labels as specified: Standard (about N minutes), Smoke v1 (about N minutes).
      const label = p => p.name === 'standard' ? `Standard (about ${p.estimate_min} minutes)` : `${p.name.charAt(0).toUpperCase() + p.name.slice(1)} v${p.version} (about ${p.estimate_min} minutes)`;
      sel.innerHTML = monProfiles.map(p => `<option value="${esc(p.name)}">${esc(label(p))}</option>`).join('');
      if (keep) sel.value = keep;
    }
    const [ready, st, rep] = await Promise.all([api('/api/testrun/readiness'), api('/api/testrun/status'), api('/api/testrun/report')]);
    lastRunStatus = st;
    renderReadiness(ready);
    renderControls(st, ready.ready);
    renderShowcase();
    renderReport(rep, st);
    // The list changes when a run saves a report or its upload state moves.
    const key = `${rep.report_id || ''}:${rep.state || ''}`;
    if (full || key !== lastReportKey) { lastReportKey = key; refreshReportList(); }
  } catch (e) {
    document.getElementById('mon-start-status').textContent = 'Error: ' + e.message;
  }
}

let lastReportKey = null;

// ---------- SW Host Benchmark: previous reports ----------

async function refreshReportList() {
  const list = document.getElementById('report-list');
  try {
    const {reports} = await api('/api/reports');
    document.getElementById('reports-count').textContent = reports.length;
    if (!reports.length) {
      list.innerHTML = '<p class="status-line">No reports yet. A report is saved when a test script run completes.</p>';
      return;
    }
    const states = {saved: 'saved locally', uploading: 'uploading', uploaded: 'uploaded to DT Collector', failed: 'upload failed', none: 'saved locally'};
    list.innerHTML = reports.map(r => `
      <div class="log-row">
        <div class="log-row-info">
          <div class="log-row-name">${esc(r.profile)} · ${new Date(r.created_at).toLocaleString()}</div>
          <div class="log-row-meta">${esc(r.cpu_model)} · ${r.tests} tests · ${esc(states[r.upload_state] || r.upload_state)} · ${formatBytes(r.size_bytes)}</div>
        </div>
        <div class="log-row-actions">
          <button class="btn secondary" onclick="viewReport('${esc(r.id)}')">View</button>
          <button class="btn secondary" onclick="downloadReport('${esc(r.id)}')">Download</button>
          <button class="btn danger" onclick="deleteReport('${esc(r.id)}')">Delete</button>
        </div>
      </div>`).join('');
  } catch (e) {
    list.innerHTML = `<p class="status-line error">Error loading reports: ${esc(e.message)}</p>`;
  }
}

function viewReport(id) {
  window.open(`/api/reports/view?id=${encodeURIComponent(id)}`, '_blank');
}

function downloadReport(id) {
  window.open(`/api/reports/download?id=${encodeURIComponent(id)}`, '_blank');
}

async function deleteReport(id) {
  if (!confirm('Delete this report? Only the local copy is deleted; a report already uploaded stays in DT Collector.')) return;
  try {
    await api('/api/reports/delete', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id})});
  } catch (e) {
    alert('Delete failed: ' + e.message);
  }
  refreshReportList();
}

let readyAutoSet = false;
function renderReadiness(ready) {
  document.getElementById('mon-checks').innerHTML = ready.checks.map(c => {
    const cls = c.ok ? 'ok' : (c.warn ? 'warn' : 'bad');
    const mark = c.ok ? '✓' : (c.warn ? '⚠' : '✗');
    return `<li><span class="${cls}">${mark}</span> ${esc(c.label)}<span class="note">${esc(c.note)}</span></li>`;
  }).join('');
  const ok = ready.checks.filter(c => c.ok).length;
  const badge = document.getElementById('ready-count');
  badge.textContent = `${ok}/${ready.checks.length} ready`;
  badge.classList.toggle('badge-warn', ok < ready.checks.length);
  // Open by default only when something needs attention.
  if (!readyAutoSet) { setSectionOpen('ready', !ready.ready); readyAutoSet = true; }
}

function renderControls(st, ready) {
  const running = !!st.running;
  document.getElementById('mon-start-btn').disabled = running || !ready;
  document.getElementById('mon-cancel-btn').classList.toggle('hidden', !running);
  document.getElementById('mon-profile').disabled = running;
}

function toggleProfileInfo(ev) {
  ev.stopPropagation();
  const pop = document.getElementById('mon-info-pop');
  const open = pop.classList.contains('hidden');
  if (open) {
    const name = document.getElementById('mon-profile').value;
    pop.innerHTML = PROFILE_INFO[name] || '<p>No description for this profile.</p>';
  }
  pop.classList.toggle('hidden', !open);
  document.getElementById('mon-info-btn').setAttribute('aria-expanded', String(open));
}
// Hover shows it too; clicking elsewhere closes a pinned popover.
document.addEventListener('DOMContentLoaded', () => {
  const wrap = document.querySelector('.info-wrap');
  if (!wrap) return;
  let pinned = false;
  const pop = document.getElementById('mon-info-pop');
  const show = () => { pop.innerHTML = PROFILE_INFO[document.getElementById('mon-profile').value] || ''; pop.classList.remove('hidden'); };
  wrap.addEventListener('mouseenter', () => { if (!pinned) show(); });
  wrap.addEventListener('mouseleave', () => { if (!pinned) pop.classList.add('hidden'); });
  document.getElementById('mon-info-btn').addEventListener('click', () => { pinned = !pop.classList.contains('hidden'); });
  document.addEventListener('click', (e) => { if (!wrap.contains(e.target)) { pinned = false; pop.classList.add('hidden'); } });
});

function describeTest(t) {
  const codec = t.caller_codec === t.callee_codec ? t.caller_codec : `${t.caller_codec} → ${t.callee_codec} (transcoding)`;
  const rec = {off: 'no recording', mono: 'mono recording', stereo: 'stereo recording'}[t.recording] || t.recording;
  const load = t.mode === 'ramp'
    ? `ramp to ${t.target_calls} calls, hold ${Math.round(t.hold_ns / 1e9)}s`
    : `rolling ${Math.round(t.call_duration_ns / 1e9)}s calls at ${t.rolling_cps.join(' / ')} calls/s`;
  const title = t.mode === 'rolling'
    ? `Rolling calls, ${rec}`
    : `${rec.charAt(0).toUpperCase() + rec.slice(1)}, ${t.caller_codec === t.callee_codec ? 'low-cost codec' : 'high-cost codec'}`;
  return {codec, rec, load, title};
}

// renderShowcase draws the progress bar, the step tracker and, while a run
// is going (or after it), the live figures and whole-run charts.
function renderShowcase() {
  const st = lastRunStatus || {};
  const selected = document.getElementById('mon-profile').value;
  const runProfile = st.profile ? st.profile.split(' ')[0] : null;
  // Show the run's own profile once one has started; otherwise the selection.
  const pname = (st.running || st.done) && runProfile ? runProfile : selected;
  const p = monProfiles.find(x => x.name === pname);
  if (!p) return;
  const running = !!st.running, started = !!st.started_at && runProfile === pname;

  document.getElementById('show-title').textContent = started
    ? `${pname === 'standard' ? 'Standard' : 'Smoke v1'} benchmark` + (running ? ' (running)' : st.done ? ' (finished)' : '')
    : `${pname === 'standard' ? 'Standard' : 'Smoke v1'} benchmark (not started)`;

  // Overall progress: elapsed against the profile's estimate.
  const fill = document.getElementById('run-progress-fill');
  const text = document.getElementById('run-progress-text');
  if (started) {
    const elapsed = ((st.done && st.timeline && st.timeline.length ? st.timeline[st.timeline.length - 1].t * 1000 : Date.now()) - new Date(st.started_at)) / 1000;
    const est = st.estimated_sec || p.estimate_min * 60;
    const done = st.done ? 1 : Math.min(0.99, elapsed / est);
    fill.style.width = (done * 100).toFixed(1) + '%';
    const m = s => `${Math.floor(s / 60)} min`;
    text.textContent = running
      ? `Test ${st.test_index} of ${st.test_count} · ${PHASES[st.phase] || st.phase || 'starting'} · ${m(elapsed)} elapsed, about ${m(Math.max(0, est - elapsed))} left`
      : (st.done ? `Finished after ${m(elapsed)}` + (st.error ? ` · ${st.error}` : '') : '');
  } else {
    fill.style.width = '0%';
    text.textContent = `${p.tests.length} tests · about ${p.estimate_min} minutes`;
  }

  // Step tracker: finished and future steps greyed, the current one bold.
  const results = started ? (st.results || []) : [];
  const cur = running ? st.test_index : 0;
  document.getElementById('show-steps').innerHTML = p.tests.map((t, i) => {
    const n = i + 1, d = describeTest(t), r = results[i];
    const state = n === cur ? 'current' : (r ? 'done' : 'future');
    let extra = '';
    if (state === 'current') {
      const order = PHASE_ORDER[t.mode] || [];
      const at = order.indexOf(st.phase);
      extra = `<div class="phases">${order.map((ph, j) => `<span class="phase ${j < at ? 'past' : j === at ? 'now' : ''}">${esc(PHASES[ph])}</span>`).join('<span class="sep">›</span>')}</div>`;
    } else if (r) {
      const ok = r.stop_reason === 'target_reached';
      extra = `<div class="step-result"><span class="${ok ? 'ok' : 'warn'}">${ok ? '✓' : '⚠'}</span> ${esc(STOP_REASONS[r.stop_reason] || r.stop_reason)} · max ${r.max_concurrent_calls} calls` +
        (r.at_load && r.at_load.host_cpu_pct !== undefined ? ` · host CPU ${fmt1(r.at_load.host_cpu_pct)}%` : '') + '</div>';
    }
    return `<li class="step ${state}"><span class="step-num">${state === 'done' ? '✓' : n}</span><div class="step-body">` +
      `<div class="step-title">${esc(d.title)}</div><div class="step-desc">${esc(d.load)} · ${esc(d.codec)} · ${esc(d.rec)}</div>${extra}</div></li>`;
  }).join('');

  document.getElementById('show-hero').classList.toggle('hidden', !running || !st.latest);
  document.getElementById('show-charts').classList.toggle('hidden', !started || !(st.timeline || []).length);
  document.getElementById('mon-run-panel').classList.toggle('hidden', !started);
  if (running && st.latest) renderHero(st, p);
  if (started) {
    renderCharts(st, p);
    renderResults(st);
  }
}

function renderHero(st, p) {
  const s = st.latest, vps = s.vps || {};
  const t = p.tests[st.test_index - 1] || {};
  const target = t.target_calls || 0;
  document.getElementById('hero-calls').textContent = s.concurrent_calls;
  document.getElementById('hero-target').textContent = target ? ` / ${target}` : '';
  document.getElementById('hero-calls-bar').style.width = target ? Math.min(100, 100 * s.concurrent_calls / target) + '%' : '0%';
  const cpus = st.host_cpus || 1;
  const g = [
    ['Host CPU', s.host.cpu_pct, '%', 100],
    ['Host memory', s.host.mem_pct, '%', 100],
    ['MT VPS CPU', vps.MT && vps.MT.cpu_pct / cpus, '% of host', 100],
    ['CC VPS CPU', vps.CC && vps.CC.cpu_pct / cpus, '% of host', 100],
    ['MT Asterisk', vps.MT && vps.MT.asterisk_cpu_pct, '% of a core', null],
    ['CC Asterisk', vps.CC && vps.CC.asterisk_cpu_pct, '% of a core', null],
    ['SwarmDialer CPU', s.swarmdialer.cpu_pct, '%', 100],
    ['Setup p95', s.setup_p95_ms, 'ms', null],
    ['Answered / failed', null, `${s.answered} / ${s.failed}`, null],
    ['Peak calls (run)', st.peak_calls, '', null],
    ['Peak host CPU (run)', st.peak_host_cpu_pct, '%', 100],
  ];
  if (s.ramdisk_est_mb) g.push(['RAM disk (est.)', s.ramdisk_est_mb, 'MB', null]);
  if (s.cps) g.push(['Dial rate', s.cps, 'calls/s', null]);
  document.getElementById('show-gauges').innerHTML = g.map(([k, v, unit, max]) => {
    const val = v === null ? unit : `${fmt1(v)}<span class="u">${esc(unit)}</span>`;
    const bar = max ? `<div class="meter sm"><div style="width:${Math.min(100, Math.max(0, v || 0))}%"></div></div>` : '';
    return `<div class="gauge"><div class="gv">${val}</div><div class="gk">${esc(k)}</div>${bar}</div>`;
  }).join('');
}

function renderCharts(st, p) {
  applySeriesColors();
  const tl = st.timeline || [];
  const cpus = st.host_cpus || 1, gb = 1024 ** 3;
  const target = Math.max(...p.tests.map(t => t.target_calls || 0));
  const legend = (id) => { document.getElementById(id).innerHTML = SERIES.map(s => `<span class="leg"><i style="background:${s.color}"></i>${esc(s.label)}</span>`).join(''); };
  legend('leg-cpu'); legend('leg-mem');
  drawTimeline('chart-cpu', tl, [
    {...SERIES[0], v: x => x.host_cpu},
    {...SERIES[1], v: x => (x.vps_cpu.MT || 0) / cpus},
    {...SERIES[2], v: x => (x.vps_cpu.CC || 0) / cpus},
  ], {max: 100, unit: '%'});
  drawTimeline('chart-mem', tl, [
    {...SERIES[0], v: x => (st.host_mem_bytes || 0) * x.host_mem / 100 / gb},
    {...SERIES[1], v: x => (x.vps_mem.MT || 0) / gb},
    {...SERIES[2], v: x => (x.vps_mem.CC || 0) / gb},
  ], {max: st.host_mem_bytes ? st.host_mem_bytes / gb : null, unit: ' GB'});
  drawTimeline('chart-calls', tl, [{key: 'calls', label: 'Calls at once', color: SERIES[0].color, v: x => x.calls}], {max: Math.max(target, 10), unit: '', target});
}

// drawTimeline draws a whole-run line chart sized to its container, with
// test boundaries, direct end labels (>= 2 series), and a hover crosshair
// listing every series' value.
function drawTimeline(id, tl, series, opts) {
  const svg = document.getElementById(id);
  const plot = svg.parentElement;
  const W = Math.max(300, plot.clientWidth), H = 190, L = 40, R = series.length > 1 ? 118 : 12, T = 10, B = 22;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('width', W); svg.setAttribute('height', H);
  if (!tl.length) { svg.innerHTML = ''; return; }
  const t0 = tl[0].t, t1 = Math.max(tl[tl.length - 1].t, t0 + 60);
  let max = opts.max;
  if (!max) max = Math.max(1, ...tl.flatMap(p => series.map(s => s.v(p) || 0))) * 1.15;
  const x = t => L + (t - t0) * (W - L - R) / (t1 - t0);
  const y = v => T + (H - T - B) * (1 - Math.min(v, max) / max);
  let g = '';
  [0, 0.5, 1].forEach(f => {
    const v = max * f;
    g += `<line class="grid" x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}"/><text class="axis" x="${L - 6}" y="${y(v) + 4}" text-anchor="end">${fmt1(v)}</text>`;
  });
  // Test boundaries, labelled with the test number.
  let prev = null;
  tl.forEach(pt => {
    if (pt.test !== prev) {
      g += `<line class="bound" x1="${x(pt.t)}" x2="${x(pt.t)}" y1="${T}" y2="${H - B}"/><text class="axis" x="${x(pt.t) + 3}" y="${H - 6}">${pt.test}</text>`;
      prev = pt.test;
    }
  });
  if (opts.target) g += `<line class="target" x1="${L}" x2="${W - R}" y1="${y(opts.target)}" y2="${y(opts.target)}"/><text class="axis" x="${W - R - 4}" y="${y(opts.target) - 4}" text-anchor="end">target ${opts.target}</text>`;
  series.forEach(s => {
    const d = tl.map((pt, i) => `${i ? 'L' : 'M'}${x(pt.t).toFixed(1)},${y(s.v(pt) || 0).toFixed(1)}`).join('');
    g += `<path class="tl" style="stroke:${s.color}" d="${d}"/>`;
  });
  // Direct labels at the line ends (text in text ink, a colour dot beside).
  if (series.length > 1) {
    const last = tl[tl.length - 1];
    const ys = series.map(s => ({s, yy: y(s.v(last) || 0)})).sort((a, b) => a.yy - b.yy);
    for (let i = 1; i < ys.length; i++) if (ys[i].yy - ys[i - 1].yy < 13) ys[i].yy = ys[i - 1].yy + 13;
    ys.forEach(({s, yy}) => { g += `<circle cx="${W - R + 8}" cy="${yy - 4}" r="4" fill="${s.color}"/><text class="endlbl" x="${W - R + 16}" y="${yy}">${esc(s.label.replace('PBXware ', '').replace(' VPS', ''))} ${fmt1(s.v(last))}${esc(opts.unit)}</text>`; });
  }
  g += '<g class="hover"></g>';
  svg.innerHTML = g;
  const tip = plot.querySelector('.tip');
  svg.onmousemove = (ev) => {
    const r = svg.getBoundingClientRect();
    const px = (ev.clientX - r.left) * W / r.width;
    const tt = t0 + (px - L) * (t1 - t0) / (W - L - R);
    let best = 0;
    tl.forEach((pt, i) => { if (Math.abs(pt.t - tt) < Math.abs(tl[best].t - tt)) best = i; });
    const pt = tl[best];
    svg.querySelector('.hover').innerHTML = `<line class="cursor" x1="${x(pt.t)}" x2="${x(pt.t)}" y1="${T}" y2="${H - B}"/>` +
      series.map(s => `<circle cx="${x(pt.t)}" cy="${y(s.v(pt) || 0)}" r="4" fill="${s.color}" stroke="${cssVar('--panel-alt')}" stroke-width="2"/>`).join('');
    tip.innerHTML = `<div class="tip-h">${new Date(pt.t * 1000).toLocaleTimeString()} · test ${pt.test} · ${esc(PHASES[pt.phase] || pt.phase)}</div>` +
      series.map(s => `<div><i style="background:${s.color}"></i>${esc(s.label)}: <b>${fmt1(s.v(pt))}${esc(opts.unit)}</b></div>`).join('');
    tip.classList.remove('hidden');
    const left = Math.min(ev.clientX - r.left + 14, r.width - tip.offsetWidth - 4);
    tip.style.left = Math.max(0, left) + 'px';
    tip.style.top = '8px';
  };
  svg.onmouseleave = () => { svg.querySelector('.hover').innerHTML = ''; tip.classList.add('hidden'); };
}

function renderResults(st) {
  const results = st.results || [];
  document.getElementById('mon-results').innerHTML = results.length === 0 ? '<tr><td>No tests finished yet.</td></tr>' :
    '<tr><th>Test</th><th>Result</th><th>Max calls</th><th>Answered / failed</th><th>Setup avg / p95</th><th>Host CPU at load</th><th>Asterisk CPU MT / CC</th><th>RTP recv</th><th>MOS avg / min</th><th>Quality dropped at</th><th>MP3 delay avg (MT / CC)</th><th>RAM disk full (est.)</th></tr>' +
    results.map(r => {
      const al = r.at_load || {}; const ast = al.asterisk_cpu_pct || {};
      const rec = r.recording;
      const mp3 = rec && rec.mp3_conversion_delay_s ? `${fmt1(rec.mp3_conversion_delay_s.MT && rec.mp3_conversion_delay_s.MT.avg)}s / ${fmt1(rec.mp3_conversion_delay_s.CC && rec.mp3_conversion_delay_s.CC.avg)}s` : '–';
      const full = rec && rec.ramdisk_full_estimated_at_calls ? `at ${rec.ramdisk_full_estimated_at_calls} calls` : '–';
      const reason = (STOP_REASONS[r.stop_reason] || r.stop_reason) + (r.stop_detail ? ` (${r.stop_detail})` : '');
      const mos = r.mos && r.mos.n ? `${fmt1(r.mos.avg)} / ${fmt1(r.mos.min)}` : '–';
      return `<tr><td>${esc(r.test.id)}</td><td>${esc(reason)}</td><td>${r.max_concurrent_calls}</td><td>${r.calls.answered} / ${r.calls.failed}</td>` +
        `<td>${fmt1(r.setup_ms.avg)} / ${fmt1(r.setup_ms.p95)} ms</td><td>${fmt1(al.host_cpu_pct)}%</td><td>${fmt1(ast.MT)}% / ${fmt1(ast.CC)}%</td>` +
        `<td>${Math.round((r.rtp_received_ratio || 0) * 100)}%</td><td>${mos}</td><td>${r.quality_degraded_at_calls ? r.quality_degraded_at_calls + ' calls' : '–'}</td><td>${esc(mp3)}</td><td>${esc(full)}</td></tr>`;
    }).join('');
  const log = document.getElementById('mon-log');
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 20;
  log.textContent = (st.log || []).join('\n');
  if (atBottom) log.scrollTop = log.scrollHeight;
}

// Present: the showcase alone, full screen, for leaving on a screen
// during a long run.
function togglePresent() {
  const el = document.getElementById('mon-show');
  if (document.fullscreenElement) document.exitFullscreen();
  else if (el.requestFullscreen) el.requestFullscreen().then(() => renderShowcase());
}
document.addEventListener('fullscreenchange', () => {
  document.getElementById('show-present-btn').textContent = document.fullscreenElement ? 'Exit' : 'Present';
  setTimeout(renderShowcase, 50); // charts re-measure their width
});
window.addEventListener('resize', () => { if (!document.getElementById('tab-monitoring').classList.contains('hidden')) renderShowcase(); });

async function startTestRun() {
  const btn = document.getElementById('mon-start-btn');
  const profile = document.getElementById('mon-profile').value;
  const p = monProfiles.find(x => x.name === profile);
  if (!confirm(`Start the ${profile} test script? It takes about ${p ? p.estimate_min : '?'} minutes, places real call load on both PBXware instances, and pauses manual dialing until it finishes.`)) return;
  btn.disabled = true;
  const statusEl = document.getElementById('mon-start-status');
  statusEl.textContent = 'Starting...';
  statusEl.className = 'status-line';
  try {
    await api('/api/testrun/start', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({profile})});
    statusEl.textContent = '';
    refreshMonitoring(false);
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
    btn.disabled = false;
  }
}

async function cancelTestRun() {
  if (!confirm("Cancel the test run? The current test's calls are hung up and no further tests run.")) return;
  await api('/api/testrun/cancel', {method: 'POST'}).catch(() => {});
  refreshMonitoring(false);
}

let wipeDismissedFor = null;

function renderReport(rep, st) {
  const box = document.getElementById('mon-report');
  if (!rep || !rep.state || (st && st.running)) { box.classList.add('hidden'); return; }
  box.classList.remove('hidden');
  const labels = {none: 'No report', saved: 'Saved locally', uploading: 'Uploading', uploaded: 'Uploaded', failed: 'Upload failed'};
  document.getElementById('mon-report-line').textContent =
    `${labels[rep.state] || rep.state}` + (rep.report_id ? ` (report ${rep.report_id})` : '') + (rep.message ? `: ${rep.message}` : '');
  const canRetry = rep.report_id && rep.profile === 'standard' && (rep.state === 'failed' || rep.state === 'saved');
  document.getElementById('mon-report-retry').classList.toggle('hidden', !canRetry);
  document.getElementById('mon-wipe').classList.toggle('hidden', !(rep.state === 'uploaded' && wipeDismissedFor !== rep.report_id));
  if (rep.state === 'uploaded') wipeReportID = rep.report_id;
}

let wipeReportID = null;

async function retryReportUpload() {
  try {
    await api('/api/testrun/report/upload', {method: 'POST'});
  } catch (e) {
    alert('Retry failed: ' + e.message);
  }
  refreshMonitoring(false);
}

function dismissWipe() {
  wipeDismissedFor = wipeReportID || 'dismissed';
  document.getElementById('mon-wipe').classList.add('hidden');
}

async function finishAndWipe() {
  if (!confirm("Delete SwarmDialer's configuration and all stored keys now? This cannot be undone.")) return;
  try {
    await api('/api/settings/finish-wipe', {method: 'POST'});
    alert('Wiped. SwarmDialer is restarting; run the Setup Wizard again to test another host.');
    setTimeout(() => location.reload(), 3000);
  } catch (e) {
    alert('Wipe failed: ' + e.message);
  }
}

// ---------- Settings: account security ----------

async function changePassword(ev) {
  ev.preventDefault();
  const btn = document.getElementById('pw-btn');
  const statusEl = document.getElementById('pw-status');
  const cur = document.getElementById('pw-current');
  const nw = document.getElementById('pw-new');
  const conf = document.getElementById('pw-confirm');
  if (nw.value !== conf.value) {
    statusEl.textContent = "The new password and its confirmation don't match.";
    statusEl.className = 'status-line error';
    return;
  }
  btn.disabled = true;
  statusEl.textContent = 'Changing...';
  statusEl.className = 'status-line';
  try {
    await api('/api/account/password', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({current: cur.value, new: nw.value, confirm: conf.value}),
    });
    cur.value = nw.value = conf.value = '';
    statusEl.textContent = 'Password changed. Other sessions were logged out.';
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  btn.disabled = false;
}

// ---------- SW Host Benchmark: hardware info only ----------

async function uploadHardwareInfo() {
  const btn = document.getElementById('hw-upload-btn');
  const statusEl = document.getElementById('hw-upload-status');
  btn.disabled = true;
  statusEl.textContent = 'Collecting hardware info and uploading...';
  statusEl.className = 'status-line';
  try {
    const r = await api('/api/reports/hardware', {method: 'POST'});
    statusEl.textContent = (r.state === 'uploaded' ? 'Uploaded' : r.state === 'failed' ? 'Upload failed' : 'Saved locally') + ': ' + r.message;
    statusEl.className = r.state === 'failed' ? 'status-line error' : 'status-line';
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  btn.disabled = false;
  refreshReportList();
}
