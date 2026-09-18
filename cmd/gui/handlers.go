package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/sipua"
	"swarmdialer/internal/store"
	"swarmdialer/internal/wizard"
)

var (
	errServerNotFound   = errors.New("server not found")
	errNeedSecondServer = errors.New("a second server must be connected before remote dialing")
	errNeedServer       = errors.New("no server configured yet — finish the setup wizard first")
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
	Package        string `json:"package"`
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
			ExtLength: req.ExtLength, Package: req.Package,
			Country: req.Country, National: req.National, International: req.International,
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
		ID          string                          `json:"id"`
		Name        string                          `json:"name"`
		BaseURL     string                          `json:"base_url"`
		Edition     string                          `json:"edition"`
		TenantID    int                             `json:"tenant_id"`
		TenantCode  string                          `json:"tenant_code"`
		ExtCount    int                             `json:"extension_count"`
		TrunkID     int                             `json:"trunk_id,omitempty"`
		PeerServerID string                         `json:"peer_server_id,omitempty"`
		DIDCount    int                             `json:"did_count"`
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
	Section            string `json:"section"` // "local" or "remote"
	Count              int    `json:"count"`
	CallDurationSeconds int   `json:"call_duration_seconds"`
	UseRTP             bool   `json:"use_rtp"`
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

	sess, err := a.sessionFor(req.Section)
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

// sessionFor returns the session for "local" or "remote", creating it
// lazily from the store's configured server(s) on first use.
func (a *app) sessionFor(section string) (*orchestrator.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if section == "remote" {
		if a.remoteSession != nil {
			return a.remoteSession, nil
		}
		servers := a.store.Servers()
		if len(servers) < 2 {
			return nil, errNeedSecondServer
		}
		srv1, srv2 := servers[0], servers[1]
		sess, err := orchestrator.NewRemoteSession(a.ctx, orchestrator.RemoteSessionConfig{
			CallerDialDestination: srv1.SIPHost, CallerSIPDomain: srv1.SIPHost,
			CalleeDialDestination: srv2.SIPHost, CalleeSIPDomain: srv2.SIPHost,
			LocalIP:         srv1.LocalIP,
			CallerBasePort:  a.allocPortRangeLocked(len(srv1.Extensions)),
			CalleeBasePort:  a.allocPortRangeLocked(len(srv2.Extensions)),
			RegisterTimeout: 15 * time.Second,
			DialTimeout:     15 * time.Second,
		}, toEndpoints(srv1.Extensions), toEndpoints(srv2.Extensions), didLookup(srv2))
		if err != nil {
			return nil, err
		}
		a.remoteSession = sess
		return sess, nil
	}

	if a.localSession != nil {
		return a.localSession, nil
	}
	servers := a.store.Servers()
	if len(servers) < 1 {
		return nil, errNeedServer
	}
	srv := servers[0]
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
	a.localSession = sess
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
	section := r.URL.Query().Get("section")
	a.mu.Lock()
	var sess *orchestrator.Session
	if section == "remote" {
		sess = a.remoteSession
	} else {
		sess = a.localSession
	}
	a.mu.Unlock()

	if sess == nil {
		writeJSON(w, http.StatusOK, orchestrator.SessionSnapshot{})
		return
	}
	writeJSON(w, http.StatusOK, sess.Snapshot())
}
