package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

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
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
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
	result, err := wizard.TestConnection(req.BaseURL, req.APIKey)
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
	APIKey         string `json:"api_key"`
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
			Name: req.Name, BaseURL: req.BaseURL, APIKey: req.APIKey,
			ExtensionCount: req.ExtensionCount,
			TenantCode:     req.TenantCode, TenantName: req.TenantName,
			ExtLength: req.ExtLength,
			Country:   req.Country, National: req.National, International: req.International,
			ChannelLimit: 600, MaxWait: 10 * time.Minute,
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

	callDuration := time.Duration(req.CallDurationSeconds) * time.Second
	// 200ms was too aggressive at scale — a live GUI test with 200
	// cumulative calls saw 168 "603 Decline" failures (see
	// PROJECT_STATE.md's INVITE-burst-sensitivity finding). 800ms matches
	// the last empirically-safe value found during small-scale testing;
	// re-tune from real data as testing moves to higher call counts.
	const rampInterval = 800 * time.Millisecond
	started := sess.AddCalls(req.Count, callDuration, rampInterval, req.UseRTP)
	writeJSON(w, http.StatusOK, map[string]int{"started": started})
}

// sessionFor returns the session for dialing from serverID — for "local"
// section, extensions on serverID call each other directly; for "remote",
// they dial into peerServerID's extensions over the trunk between them.
// Sessions are created lazily and cached per server (or per connected
// pair), so multiple servers/pairs can each have their own live session
// at once rather than sharing one implicit "the local server" slot.
func (a *app) sessionFor(section, serverID, peerServerID string) (*orchestrator.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if serverID == "" {
		return nil, errNoServerChosen
	}
	srv := a.store.GetServer(serverID)
	if srv == nil {
		return nil, errServerNotFound
	}

	if section == "remote" {
		if peerServerID == "" {
			return nil, errNoPeerChosen
		}
		peer := a.store.GetServer(peerServerID)
		if peer == nil {
			return nil, errServerNotFound
		}
		if srv.PeerServerID != peer.ID {
			return nil, errNotConnected
		}

		key := serverID + "|" + peerServerID
		if sess, ok := a.remoteSessions[key]; ok {
			return sess, nil
		}
		sess, err := orchestrator.NewRemoteSession(a.ctx, orchestrator.RemoteSessionConfig{
			CallerDialDestination: srv.SIPHost, CallerSIPDomain: srv.SIPHost,
			CalleeDialDestination: peer.SIPHost, CalleeSIPDomain: peer.SIPHost,
			LocalIP:         srv.LocalIP,
			CallerBasePort:  a.allocPortRangeLocked(len(srv.Extensions)),
			CalleeBasePort:  a.allocPortRangeLocked(len(peer.Extensions)),
			RegisterTimeout: 15 * time.Second,
			DialTimeout:     15 * time.Second,
		}, toEndpoints(srv.Extensions), toEndpoints(peer.Extensions), didLookup(peer))
		if err != nil {
			return nil, err
		}
		a.remoteSessions[key] = sess
		return sess, nil
	}

	if sess, ok := a.localSessions[serverID]; ok {
		return sess, nil
	}
	sess, err := orchestrator.NewSession(a.ctx, orchestrator.SessionConfig{
		DialDestination: srv.SIPHost, SIPDomain: srv.SIPHost,
		LocalIP:         srv.LocalIP,
		BaseLocalPort:   a.allocPortRangeLocked(len(srv.Extensions)),
		RegisterTimeout: 15 * time.Second,
		DialTimeout:     15 * time.Second,
	}, toEndpoints(srv.Extensions))
	if err != nil {
		return nil, err
	}
	a.localSessions[serverID] = sess
	return sess, nil
}

// allocPortRangeLocked is allocPortRange without re-locking a.mu — callers
// must already hold it (see sessionFor).
func (a *app) allocPortRangeLocked(n int) int {
	base := 20000 + a.nextPortOffset
	a.nextPortOffset += n + 10
	return base
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
	writeJSON(w, http.StatusOK, map[string]any{"sessions": a.allSnapshots()})
}

// --- Settings: reset ---

type resetInstanceRequest struct {
	ServerID string `json:"server_id"`
}

// handleResetInstance tears down everything SwarmDialer created on one
// PBXware instance (see wizard.ResetInstance) and removes it from the
// store. Best-effort on the PBXware side — a partial failure (reported as
// a "warning" in the response, not an HTTP error) still results in the
// server being removed from SwarmDialer, since staying "connected" to an
// instance whose trunk/tenant might already be half-deleted isn't useful
// either way.
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

	warnings := wizard.ResetInstance(srv)

	a.mu.Lock()
	delete(a.localSessions, srv.ID)
	for key := range a.remoteSessions {
		callerID, calleeID, _ := strings.Cut(key, "|")
		if callerID == srv.ID || calleeID == srv.ID {
			delete(a.remoteSessions, key)
		}
	}
	a.mu.Unlock()

	// The peer side's own trunk (a separate PBXware object on a separate
	// instance) isn't touched by resetting srv — only srv's "connected to
	// X" bookkeeping is stale now, so just clear that reference rather
	// than deleting anything real on the peer.
	if srv.PeerServerID != "" {
		if peer := a.store.GetServer(srv.PeerServerID); peer != nil {
			peer.PeerServerID = ""
			_ = a.store.UpdateServer(peer)
		}
	}

	if err := a.store.RemoveServer(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp := map[string]any{"success": true}
	if len(warnings) > 0 {
		resp["warning"] = strings.Join(warnings, "; ")
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleResetSwarmDialer deletes the persisted config file and restarts
// the GUI process in place (see restartProcess) — the equivalent of a
// fresh deployment with no server configured, without touching anything
// on PBXware itself. The restart happens after the response is sent (in a
// short-delayed goroutine) so the browser actually sees success before
// the process image is replaced.
func (a *app) handleResetSwarmDialer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := os.Remove(a.configPath); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})

	go func() {
		time.Sleep(300 * time.Millisecond)
		restartProcess()
	}()
}
