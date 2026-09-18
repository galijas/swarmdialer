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
  document.getElementById('tab-btn-wizard').classList.toggle('active', name === 'wizard');
  document.getElementById('tab-btn-dashboard').classList.toggle('active', name === 'dashboard');
  if (name === 'dashboard') {
    refreshServerList();
    connectStatusSocket();
  }
}

function showStep(id) {
  ['step-connect-1', 'step-provision-1', 'step-add-another', 'step-connect-2', 'step-provision-2', 'step-connect-trunk']
    .forEach(s => document.getElementById(s).classList.toggle('hidden', s !== id));
}

// ---------- API helpers ----------

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
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
  const statusEl = document.getElementById(prefix + '-status');
  statusEl.textContent = 'Testing connection...';
  statusEl.className = 'status-line';
  try {
    const req = {
      base_url: document.getElementById(prefix + '-url').value.trim(),
      api_key: document.getElementById(prefix + '-key').value.trim(),
    };
    const result = await api('/api/wizard/test-connection', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(req),
    });
    statusEl.textContent = `Connected. Edition: ${result.edition}. License limits — Extensions: ${result.extensions}, Tenants: ${result.tenants}, DIDs: ${result.dids}, VOIP Trunks: ${result.voip_trunks}, Channels: ${result.channels}.`;

    const pIdx = step;
    const editionNote = document.getElementById('p' + pIdx + '-edition-note');
    const tenantFields = document.getElementById('p' + pIdx + '-tenant-fields');
    const isMultiTenant = result.edition.toLowerCase() === 'multi-tenant';
    tenantFields.classList.toggle('hidden', !isMultiTenant);
    editionNote.textContent = isMultiTenant
      ? 'Multi-Tenant edition detected — a tenant will be created.'
      : `"${result.edition}" edition detected — tenant creation is skipped; extensions are created at the system level.`;

    window['pending' + step] = req; // stash for provision() to reuse
    showStep('step-provision-' + step);
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
}

// ---------- Wizard: provision ----------

async function provision(step) {
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
    extension_count: parseInt(document.getElementById('p' + step + '-ext-count').value, 10),
    tenant_code: document.getElementById('p' + step + '-tenant-code').value.trim(),
    tenant_name: document.getElementById('p' + step + '-tenant-name').value.trim(),
    ext_length: 3, package: '1', country: '869', national: '1', international: '011',
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
        return true;
      }
      if (p.done) {
        statusEl.textContent = `Done — ${p.created} extensions created.`;
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
  }
}

function startSecondServer() {
  wizardState.addingSecond = true;
  showStep('step-connect-2');
}

async function finishWizard() {
  showTab('dashboard');
}

// ---------- Wizard: connect trunk + DIDs ----------

async function connectServers() {
  const statusEl = document.getElementById('ct-status');
  const barWrap = document.getElementById('ct-progress-bar');
  const bar = barWrap.querySelector('div');
  barWrap.classList.remove('hidden');
  statusEl.textContent = 'Fetching server list...';

  const {servers} = await api('/api/servers');
  if (servers.length < 2) {
    statusEl.textContent = 'Error: need two provisioned servers first.';
    statusEl.className = 'status-line error';
    return;
  }
  const server1 = servers[servers.length - 2];
  const server2 = servers[servers.length - 1];

  try {
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
        return true;
      }
      if (p.done) {
        statusEl.textContent = 'Trunk and DIDs created.';
        document.getElementById('ct-complete-btn').classList.remove('hidden');
        return true;
      }
      return false;
    });
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
}

// ---------- Dashboard: server list ----------

async function refreshServerList() {
  const {servers} = await api('/api/servers');
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
  const remotePanel = document.getElementById('remote-dialer-panel');
  remotePanel.classList.toggle('disabled-overlay', servers.length < 2);
}

// ---------- Dashboard: dialing ----------

function confirmDial(section, count) {
  if (!confirm(`Start ${count} additional ${section} call(s)?`)) return;
  const duration = parseInt(document.getElementById(section + '-duration').value, 10);
  const useRTP = document.getElementById(section + '-rtp').checked;
  api('/api/dial', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({section, count, call_duration_seconds: duration, use_rtp: useRTP}),
  }).then(r => {
    logLine(`Requested +${count} ${section} calls — ${r.started} started (pool availability may limit this).`);
  }).catch(e => logLine('Error: ' + e.message, true));
}

// ---------- Dashboard: live status ----------

let currentSection = 'local';
let ws = null;
let wsReconnectTimer = null;

function switchSection(section) {
  currentSection = section;
  lastSeenEventSeq = 0; // local/remote sessions have independent event sequences
  connectStatusSocket();
}

function setStatusView(view) {
  document.getElementById('view-btn-log').classList.toggle('active', view === 'log');
  document.getElementById('view-btn-graphic').classList.toggle('active', view === 'graphic');
  document.getElementById('call-log').classList.toggle('hidden', view !== 'log');
  document.getElementById('call-graphic').classList.toggle('hidden', view !== 'graphic');
}

function connectStatusSocket() {
  if (wsReconnectTimer) { clearTimeout(wsReconnectTimer); wsReconnectTimer = null; }
  if (ws) { ws.onclose = null; ws.close(); ws = null; }
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws/status?section=${currentSection}`);
  ws.onmessage = (ev) => {
    const snap = JSON.parse(ev.data);
    renderSnapshot(snap);
  };
  // Without this, a dropped connection (GUI server restart, network blip)
  // leaves the dashboard silently frozen — stats/log/graphic all stop
  // updating with no visible sign anything is wrong, since the one-off
  // "N started" log line comes from the /api/dial response, not the socket.
  ws.onclose = () => {
    wsReconnectTimer = setTimeout(connectStatusSocket, 2000);
  };
}

let lastSeenEventSeq = 0;

function renderSnapshot(snap) {
  document.getElementById('stat-active').textContent = (snap.active_calls || []).length;
  document.getElementById('stat-started').textContent = snap.total_calls_started || 0;
  document.getElementById('stat-answered').textContent = snap.total_calls_answered || 0;
  document.getElementById('stat-failed').textContent = snap.total_calls_failed || 0;
  document.getElementById('stat-rtp').textContent = (snap.total_rtp_sent || 0) + (snap.total_rtp_recv || 0);

  // Log view: one line per lifecycle event (dialing/answered/failed/ended),
  // per extension pair — recent_events is a server-side ring buffer, we
  // only log the ones newer than the last one we've already shown.
  (snap.recent_events || []).forEach(ev => {
    if (ev.seq <= lastSeenEventSeq) return;
    lastSeenEventSeq = ev.seq;
    const pair = `${ev.caller_aor} &rarr; ${ev.callee_aor}`;
    if (ev.type === 'dialing') logLine(`dialing ${pair}`);
    else if (ev.type === 'answered') logLine(`<span class="ok">answered</span> ${pair}`);
    else if (ev.type === 'failed') logLine(`<span class="err">failed</span> ${pair} — ${ev.detail}`, true);
    else if (ev.type === 'ended') logLine(`ended ${pair} (${ev.detail})`);
    else if (ev.type === 'batch_done') logLine(`<strong>${ev.detail}</strong>`);
  });

  // Graphic view: one dot per active call, green if RTP flowing.
  const grid = document.getElementById('call-dot-grid');
  grid.innerHTML = '';
  (snap.active_calls || []).forEach(c => {
    const dot = document.createElement('div');
    dot.className = 'call-dot' + (c.rtp_sent > 0 ? ' rtp' : '');
    dot.title = `${c.caller_aor} -> ${c.callee_aor} (sent ${c.rtp_sent}, recv ${c.rtp_recv})`;
    grid.appendChild(dot);
  });
}

function logLine(html, isError) {
  const log = document.getElementById('call-log');
  const row = document.createElement('div');
  row.className = 'row' + (isError ? ' error' : '');
  const time = new Date().toLocaleTimeString();
  row.innerHTML = `[${time}] ${html}`;
  log.appendChild(row);
  log.scrollTop = log.scrollHeight;
}
