package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"swarmdialer/internal/logstore"
	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/sipua"
	"swarmdialer/internal/store"
	"swarmdialer/internal/wizard"
)

var (
	errServerNotFound = errors.New("server not found")
	errNoServerChosen = errors.New("no server selected")
	errNoPeerChosen   = errors.New("no peer server selected for remote dialing")
	errNotConnected   = errors.New("selected servers aren't connected by a trunk")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// --- Wizard: test connection ---

type testConnectionRequest struct {
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"` // legacy (v1) API key
	APIKeyV2 string `json:"api_key_v2"`
}

func (a *app) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req testConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.BaseURL = normalizeBaseURL(req.BaseURL)
	result, err := wizard.TestConnection(req.BaseURL, req.APIKey, req.APIKeyV2)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"edition":     result.License.Edition,
		"channels":    result.License.Channels,
		"extensions":  result.License.Extensions,
		"tenants":     result.License.Tenants,
		"dids":        result.License.DIDs,
		"voip_trunks": result.License.VOIPTrunks,
		"pstn_trunks": result.License.PSTNTrunks,
		"local_ip":    result.LocalIP,
	})
}

// --- Wizard: provision ---

type provisionRequest struct {
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"` // legacy (v1) API key
	APIKeyV2       string `json:"api_key_v2"`
	ExtensionCount int    `json:"extension_count"`
	TenantCode     string `json:"tenant_code"`
	TenantName     string `json:"tenant_name"`
	ExtLength      int    `json:"ext_length"`
	Country        string `json:"country"`
	National       string `json:"national"`
	International  string `json:"international"`
}

func (a *app) handleProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req provisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	progress := &wizard.ProvisionProgress{}
	jobID := a.newJobID()
	a.mu.Lock()
	a.provisionJobs[jobID] = progress
	a.mu.Unlock()

	go func() {
		srv, err := wizard.Provision(a.ctx, wizard.ProvisionParams{
			Name: req.Name, BaseURL: normalizeBaseURL(req.BaseURL), APIKey: req.APIKey, APIKeyV2: req.APIKeyV2,
			ExtensionCount: req.ExtensionCount,
			TenantCode:     req.TenantCode, TenantName: req.TenantName,
			ExtLength: req.ExtLength,
			Country:   req.Country, National: req.National, International: req.International,
			ChannelLimit: 1000, MaxWait: 10 * time.Minute,
		}, progress)
		if err != nil {
			return // progress already carries the error
		}
		_ = a.store.AddServer(srv)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (a *app) handleProvisionStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	a.mu.Lock()
	progress, ok := a.provisionJobs[jobID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown job_id", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, progress.Snapshot())
}

// --- Wizard: connect two servers (trunk + DIDs) ---

type connectRequest struct {
	Server1ID string `json:"server1_id"`
	Server2ID string `json:"server2_id"`
}

func (a *app) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req connectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	srv1 := a.store.GetServer(req.Server1ID)
	srv2 := a.store.GetServer(req.Server2ID)
	if srv1 == nil || srv2 == nil {
		writeError(w, http.StatusNotFound, errServerNotFound)
		return
	}

	progress := &wizard.ConnectProgress{}
	jobID := a.newJobID()
	a.mu.Lock()
	a.connectJobs[jobID] = progress
	a.mu.Unlock()

	go func() {
		if err := wizard.ConnectServers(srv1, srv2, progress); err != nil {
			return
		}
		_ = a.store.UpdateServer(srv1)
		_ = a.store.UpdateServer(srv2)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (a *app) handleConnectStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	a.mu.Lock()
	progress, ok := a.connectJobs[jobID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown job_id", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, progress.Snapshot())
}

// --- Servers listing ---

func (a *app) handleServers(w http.ResponseWriter, r *http.Request) {
	servers := a.store.Servers()
	// Omit API keys from what the browser sees — no reason to put them in
	// the DOM/JS console even on a no-auth internal tool.
	type serverView struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		BaseURL      string `json:"base_url"`
		Edition      string `json:"edition"`
		TenantID     int    `json:"tenant_id"`
		TenantCode   string `json:"tenant_code"`
		ExtCount     int    `json:"extension_count"`
		TrunkID      int    `json:"trunk_id,omitempty"`
		PeerServerID string `json:"peer_server_id,omitempty"`
		DIDCount     int    `json:"did_count"`
	}
	views := make([]serverView, 0, len(servers))
	for _, s := range servers {
		views = append(views, serverView{
			ID: s.ID, Name: s.Name, BaseURL: s.BaseURL, Edition: s.Edition,
			TenantID: s.TenantID, TenantCode: s.TenantCode, ExtCount: len(s.Extensions),
			TrunkID: s.TrunkID, PeerServerID: s.PeerServerID, DIDCount: len(s.DIDs),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

// --- Dialing ---

type dialRequest struct {
	Section             string `json:"section"`        // "local" or "remote"
	ServerID            string `json:"server_id"`      // which server's extensions place the calls
	PeerServerID        string `json:"peer_server_id"` // remote only: which connected peer to dial into
	Count               int    `json:"count"`
	CallDurationSeconds int    `json:"call_duration_seconds"`
	UseRTP              bool   `json:"use_rtp"`
	Codec               string `json:"codec"` // "ulaw" (default), "g722", "g729", or "opus" — see sipua.Codec
}

func (a *app) handleDial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req dialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	sess, err := a.sessionFor(req.Section, req.ServerID, req.PeerServerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	logSection, reportLabel, reportClient, reportServerID, err := a.batchReportTarget(req.Section, req.ServerID, req.PeerServerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	codec := sipua.Codec(req.Codec)
	if !sipua.IsValidCodec(codec) {
		codec = sipua.DefaultCodec
	}

	callDuration := time.Duration(req.CallDurationSeconds) * time.Second
	// 200ms was too aggressive at scale — a live GUI test with 200
	// cumulative calls saw 168 "603 Decline" failures (see
	// PROJECT_STATE.md's INVITE-burst-sensitivity finding). 800ms matches
	// the last empirically-safe value found during small-scale testing;
	// re-tune from real data as testing moves to higher call counts.
	const rampInterval = 800 * time.Millisecond
	// Captured once here, at batch start, rather than re-read when the
	// batch finishes — recording is a per-batch property (whatever it was
	// set to for these calls), not something that should reflect a toggle
	// flip made later while the batch was still running.
	recording, recordingKnown := a.currentRecordingStatus(req.Section, req.ServerID, req.PeerServerID)

	// Remote calls cross the trunk between the two instances, whose codec
	// order has to match this batch's codec — see prepareRemoteBatch.
	trunkNote, release := "", func() {}
	if req.Section == "remote" {
		srv, peer := a.store.GetServer(req.ServerID), a.store.GetServer(req.PeerServerID)
		if srv == nil || peer == nil {
			writeError(w, http.StatusBadRequest, errServerNotFound)
			return
		}
		note, rel, err := a.prepareRemoteBatch(srv, peer, string(codec))
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errTrunkCodecBusy) {
				status = http.StatusConflict
			}
			writeError(w, status, err)
			return
		}
		trunkNote, release = note, rel
	}

	started := sess.AddCalls(req.Count, callDuration, rampInterval, req.UseRTP, codec, func(records []orchestrator.CallRecord) {
		release() // every call has ended; the trunk is free for another codec
		a.reportBatch(logSection, reportLabel, reportClient, reportServerID, sess, req.Count, records, recording, recordingKnown)
	})
	if started == 0 {
		release() // no batch started, so onBatchDone never runs
	}
	writeJSON(w, http.StatusOK, map[string]any{"started": started, "trunk_note": trunkNote})
}

// sessionFor returns the session for dialing from serverID — for "local"
// section, extensions on serverID call each other directly; for "remote",
// they dial into peerServerID's extensions over the trunk between them.
// Sessions are created lazily and cached per server (or per connected
// pair), so multiple servers/pairs can each have their own live session
// at once rather than sharing one implicit "the local server" slot.
func (a *app) sessionFor(section, serverID, peerServerID string) (*orchestrator.Session, error) {
	for {
		a.mu.Lock()
		if serverID == "" {
			a.mu.Unlock()
			return nil, errNoServerChosen
		}
		srv := a.store.GetServer(serverID)
		if srv == nil {
			a.mu.Unlock()
			return nil, errServerNotFound
		}

		var (
			key, label string
			create     func(orchestrator.RegisterProgressFunc) (*orchestrator.Session, error)
			save       func(*orchestrator.Session)
		)
		if section == "remote" {
			if peerServerID == "" {
				a.mu.Unlock()
				return nil, errNoPeerChosen
			}
			peer := a.store.GetServer(peerServerID)
			if peer == nil {
				a.mu.Unlock()
				return nil, errServerNotFound
			}
			if srv.PeerServerID != peer.ID {
				a.mu.Unlock()
				return nil, errNotConnected
			}
			pairKey := serverID + "|" + peerServerID
			if sess, ok := a.remoteSessions[pairKey]; ok {
				a.mu.Unlock()
				return sess, nil
			}
			key, label = "remote:"+pairKey, srv.Name+" → "+peer.Name
			cfg := orchestrator.RemoteSessionConfig{
				CallerDialDestination: srv.SIPHost, CallerSIPDomain: srv.SIPHost,
				CalleeDialDestination: peer.SIPHost, CalleeSIPDomain: peer.SIPHost,
				LocalIP:         srv.LocalIP,
				CallerBasePort:  a.allocPortRangeLocked(len(srv.Extensions)),
				CalleeBasePort:  a.allocPortRangeLocked(len(peer.Extensions)),
				RegisterTimeout: 15 * time.Second,
				DialTimeout:     15 * time.Second,
			}
			callers, callees, dids := toEndpoints(srv.Extensions), toEndpoints(peer.Extensions), didLookup(peer)
			create = func(progress orchestrator.RegisterProgressFunc) (*orchestrator.Session, error) {
				cfg.OnRegisterProgress = progress
				return orchestrator.NewRemoteSession(a.ctx, cfg, callers, callees, dids)
			}
			save = func(sess *orchestrator.Session) { a.remoteSessions[pairKey] = sess }
		} else {
			if sess, ok := a.localSessions[serverID]; ok {
				a.mu.Unlock()
				return sess, nil
			}
			key, label = "local:"+serverID, srv.Name
			cfg := orchestrator.SessionConfig{
				DialDestination: srv.SIPHost, SIPDomain: srv.SIPHost,
				LocalIP:         srv.LocalIP,
				BaseLocalPort:   a.allocPortRangeLocked(len(srv.Extensions)),
				RegisterTimeout: 15 * time.Second,
				DialTimeout:     15 * time.Second,
			}
			endpoints := toEndpoints(srv.Extensions)
			create = func(progress orchestrator.RegisterProgressFunc) (*orchestrator.Session, error) {
				cfg.OnRegisterProgress = progress
				return orchestrator.NewSession(a.ctx, cfg, endpoints)
			}
			save = func(sess *orchestrator.Session) { a.localSessions[serverID] = sess }
		}

		// Another dial for the same session is already registering its
		// extensions: wait for that instead of registering them twice,
		// then look again (it may have failed, in which case this one
		// tries itself).
		if ch, ok := a.pendingSessions[key]; ok {
			a.mu.Unlock()
			<-ch
			continue
		}
		done := make(chan struct{})
		a.pendingSessions[key] = done
		a.mu.Unlock()

		// Registering every extension takes a while (20ms stagger per
		// extension plus PBXware's replies — ~20s+ for 1000), so it runs
		// without holding a.mu: holding it here froze the live status feed
		// (allSnapshots needs a.mu) for the whole registration.
		reg := a.startRegistration(key, section, label)
		sess, err := create(reg.update)
		reg.finish(err)

		a.mu.Lock()
		delete(a.pendingSessions, key)
		if err == nil {
			save(sess)
		}
		a.mu.Unlock()
		close(done)
		return sess, err
	}
}

// batchReportTarget resolves what reportBatch needs to log/query MOS for
// a dial request: which log section it belongs in, a human-readable
// label for the log file/live-status line, and the PBXware client +
// server/tenant ID to query CDRs against. That's always the *caller's*
// own instance — for a remote batch, a cross-instance call's CDR is
// recorded on the side that originated it, not the callee's.
func (a *app) batchReportTarget(section, serverID, peerServerID string) (logstore.Section, string, *pbxware.Client, int, error) {
	srv := a.store.GetServer(serverID)
	if srv == nil {
		return "", "", nil, 0, errServerNotFound
	}
	client := pbxware.NewClient(srv.BaseURL, srv.APIKey)
	cdrServerID := srv.TenantID
	if cdrServerID == 0 {
		cdrServerID = 1 // non-Multi-Tenant: extensions/CDRs live at system level
	}

	if section == "remote" {
		peer := a.store.GetServer(peerServerID)
		if peer == nil {
			return "", "", nil, 0, errServerNotFound
		}
		return logstore.SectionRemote, srv.Name + " to " + peer.Name, client, cdrServerID, nil
	}
	return logstore.SectionLocal, srv.Name, client, cdrServerID, nil
}

// allocPortRangeLocked is allocPortRange without re-locking a.mu — callers
// must already hold it (see sessionFor).
func (a *app) allocPortRangeLocked(n int) int {
	base := 20000 + a.nextPortOffset
	a.nextPortOffset += n + 10
	return base
}

// existingSessionFor is sessionFor without the lazy-create side effect —
// used by handleStop, which should do nothing (not silently spin up a
// brand new session/registration) when there's no live session for that
// dialer to stop.
func (a *app) existingSessionFor(section, serverID, peerServerID string) *orchestrator.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	if section == "remote" {
		return a.remoteSessions[serverID+"|"+peerServerID]
	}
	return a.localSessions[serverID]
}

// --- Dialing: stop ---

type stopRequest struct {
	Section      string `json:"section"`
	ServerID     string `json:"server_id"`
	PeerServerID string `json:"peer_server_id"`
}

// handleStop cancels every call the targeted dialer currently has in
// flight (queued/ramping or already answered — see Session.Stop) without
// tearing down the session, so the dashboard's +N buttons keep working
// normally afterward.
func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req stopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess := a.existingSessionFor(req.Section, req.ServerID, req.PeerServerID)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": false})
		return
	}
	sess.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
}

func toEndpoints(exts []pbxware.ProvisionedExtension) []sipua.Endpoint {
	out := make([]sipua.Endpoint, len(exts))
	for i, e := range exts {
		out[i] = sipua.Endpoint{Username: e.Username, Password: e.Secret, AOR: e.Ext}
	}
	return out
}

func didLookup(srv *store.Server) map[string]string {
	m := make(map[string]string, len(srv.DIDs))
	for _, d := range srv.DIDs {
		m[d.Ext] = d.DID
	}
	return m
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": a.allSnapshots(), "registrations": a.registrationSnapshots()})
}

// --- Settings: reset ---

type resetInstanceRequest struct {
	ServerID string `json:"server_id"`
}

// handleResetInstance tears down everything SwarmDialer created on one
// PBXware instance (see wizard.ResetInstance) and removes it from the
// store, in the background — a Multi-Tenant tenant delete can take
// several minutes (see resetTenantGoneWait), so this returns a job_id
// immediately for the caller to poll via handleResetInstanceStatus,
// the same pattern as provision/connect. Best-effort on the PBXware side
// — a partial failure (reported as a "warning" on the job, not an HTTP
// error) still results in the server being removed from SwarmDialer,
// since staying "connected" to an instance whose trunk/tenant might
// already be half-deleted isn't useful either way.
func (a *app) handleResetInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req resetInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	srv := a.store.GetServer(req.ServerID)
	if srv == nil {
		writeError(w, http.StatusNotFound, errServerNotFound)
		return
	}

	progress := &wizard.ResetProgress{}
	jobID := a.newJobID()
	a.mu.Lock()
	a.resetJobs[jobID] = progress
	a.mu.Unlock()

	go func() {
		wizard.ResetInstance(srv, progress)

		a.mu.Lock()
		delete(a.localSessions, srv.ID)
		for key := range a.remoteSessions {
			callerID, calleeID, _ := strings.Cut(key, "|")
			if callerID == srv.ID || calleeID == srv.ID {
				delete(a.remoteSessions, key)
			}
		}
		a.mu.Unlock()

		// The peer side's own trunk (a separate PBXware object on a
		// separate instance) isn't touched by resetting srv — only srv's
		// "connected to X" bookkeeping is stale now, so just clear that
		// reference rather than deleting anything real on the peer.
		if srv.PeerServerID != "" {
			if peer := a.store.GetServer(srv.PeerServerID); peer != nil {
				peer.PeerServerID = ""
				_ = a.store.UpdateServer(peer)
			}
		}

		if err := a.store.RemoveServer(srv.ID); err != nil {
			progress.AppendWarning(fmt.Sprintf("removing from SwarmDialer: %v", err))
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (a *app) handleResetInstanceStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	a.mu.Lock()
	progress, ok := a.resetJobs[jobID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown job_id", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, progress.Snapshot())
}

// handleResetSwarmDialer deletes the persisted config file and every call
// log, then restarts the GUI process in place (see restartProcess) — the
// equivalent of a fresh deployment with no server configured and no call
// history, without touching anything on PBXware itself. Also what
// resetAll's frontend flow ends with, so "reset all" clears logs too
// without needing separate handling. The restart happens after the
// response is sent (in a short-delayed goroutine) so the browser actually
// sees success before the process image is replaced.
func (a *app) handleResetSwarmDialer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := os.Remove(a.configPath); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.logs.DeleteAll(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})

	go func() {
		time.Sleep(300 * time.Millisecond)
		restartProcess()
	}()
}

// normalizeBaseURL trims whitespace and trailing slashes from a PBXware
// base URL as typed in the wizard ("https://10.1.101.11/" is common), so
// the stored form is consistent and path joins don't produce "//api/...".
func normalizeBaseURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}
