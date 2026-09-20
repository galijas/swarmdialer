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
  document.getElementById('tab-btn-wizard').classList.toggle('active', name === 'wizard');
  document.getElementById('tab-btn-dashboard').classList.toggle('active', name === 'dashboard');
  document.getElementById('tab-btn-settings').classList.toggle('active', name === 'settings');
  if (name === 'dashboard') {
    refreshServerList();
    connectStatusSockets();
  }
  if (name === 'settings') {
    refreshSettingsTab();
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

function confirmDial(section, count) {
  if (!confirm(`Start ${count} additional ${section} call(s)?`)) return;
  const duration = parseInt(document.getElementById(section + '-duration').value, 10);
  const useRTP = document.getElementById(section + '-rtp').checked;

  let serverID, peerServerID;
  if (section === 'remote') {
    const pairValue = document.getElementById('remote-pair-select').value;
    if (!pairValue) { logLine('Error: no connected server pair selected.', true); return; }
    [serverID, peerServerID] = pairValue.split(':');
  } else {
    serverID = document.getElementById('local-server-select').value;
    if (!serverID) { logLine('Error: no server selected.', true); return; }
  }

  api('/api/dial', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      section, server_id: serverID, peer_server_id: peerServerID,
      count, call_duration_seconds: duration, use_rtp: useRTP,
    }),
  }).then(r => {
    logLine(`Requested +${count} ${section} calls — ${r.started} started (pool availability may limit this).`);
  }).catch(e => logLine('Error: ' + e.message, true));
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
    const sessions = (JSON.parse(ev.data).sessions) || [];
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
}

// ---------- Settings: reset ----------

async function refreshSettingsTab() {
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

async function resetInstance(index) {
  const btn = document.getElementById(`reset-instance-${index + 1}-btn`);
  const serverID = btn.dataset.serverId;
  if (!serverID) return;
  const name = btn.textContent.replace('Reset ', '');
  if (!confirm(`Reset ${name}? This deletes everything SwarmDialer created on that PBXware instance (trunk, tenant/package or extensions) and cannot be undone.`)) return;

  const statusEl = document.getElementById('reset-instance-status');
  statusEl.textContent = `Resetting ${name}... (tenant deletion can take several minutes)`;
  statusEl.className = 'status-line';
  try {
    const result = await api('/api/settings/reset-instance', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({server_id: serverID}),
    });
    statusEl.textContent = result.warning ? `Done, with warnings: ${result.warning}` : `${name} reset.`;
    statusEl.className = result.warning ? 'status-line error' : 'status-line';
  } catch (e) {
    statusEl.textContent = 'Error: ' + e.message;
    statusEl.className = 'status-line error';
  }
  refreshSettingsTab();
}

async function resetSwarmDialer() {
  if (!confirm('Reset SwarmDialer to default? This deletes SwarmDialer\'s saved configuration and restarts the GUI. It does NOT touch anything on PBXware.')) return;
  const statusEl = document.getElementById('reset-swarmdialer-status');
  statusEl.textContent = 'Resetting and restarting...';
  statusEl.className = 'status-line';
  try {
    await api('/api/settings/reset-swarmdialer', {method: 'POST'});
  } catch (e) {
    // A dropped connection here is expected — the process restarts right
    // after responding.
  }
  await waitForRestart(statusEl);
}

async function resetAll() {
  if (!confirm('Reset ALL connected instances and SwarmDialer itself? This cannot be undone.')) return;
  const statusEl = document.getElementById('reset-all-status');
  statusEl.className = 'status-line';
  try {
    const {servers} = await api('/api/servers');
    for (const s of servers) {
      statusEl.textContent = `Resetting ${s.name}... (tenant deletion can take several minutes)`;
      await api('/api/settings/reset-instance', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({server_id: s.id}),
      });
    }
    statusEl.textContent = 'Resetting SwarmDialer...';
    await api('/api/settings/reset-swarmdialer', {method: 'POST'});
  } catch (e) {
    // ignore — a dropped connection is expected once the process restarts
  }
  await waitForRestart(statusEl);
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

// ---------- Init ----------
//
// The wizard is the right landing page only the first time, before any
// server is configured — every later visit, whoever's setting up the
// dashboard almost certainly wants the dashboard, not to re-walk the
// wizard. Reconfiguring is still one click away via the dashboard's
// "Reconfigure" button.
(async function init() {
  try {
    const {servers} = await api('/api/servers');
    if (servers.length > 0) showTab('dashboard');
  } catch (e) {
    // Can't reach the API — stay on the wizard (the default) rather than
    // silently failing somewhere less obvious.
  }
})();
