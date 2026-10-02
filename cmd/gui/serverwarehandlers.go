package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"swarmdialer/internal/dtcollector"
	"swarmdialer/internal/serverware"
	"swarmdialer/internal/store"
	"swarmdialer/internal/wizard"
)

type serverwareConnectRequest struct {
	Controller     string `json:"controller"`
	APIKey         string `json:"api_key"`
	DTCollectorURL string `json:"dt_collector_url"`
	DTCollectorKey string `json:"dt_collector_key"`
}

// handleServerwareConnect starts the Setup Wizard's SERVERware step (see
// wizard.ConnectServerware) in the background and returns a job ID to
// poll with handleServerwareStatus.
func (a *app) handleServerwareConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req serverwareConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Controller == "" || req.APIKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("the controller address and the SERVERware API key are required"))
		return
	}
	servers := a.store.Servers()
	if len(servers) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("add the PBXware instances first"))
		return
	}
	// The DT Collector upload key (optional here, also settable in
	// Settings) is checked and saved on its own, before SERVERware.
	if req.DTCollectorKey != "" || req.DTCollectorURL != "" {
		if err := a.saveDTCollector(req.DTCollectorURL, req.DTCollectorKey); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	progress := &wizard.ServerwareProgress{}
	jobID := a.newJobID()
	a.mu.Lock()
	a.serverwareJobs[jobID] = progress
	a.mu.Unlock()

	go func() {
		cfg, err := wizard.ConnectServerware(wizard.ServerwareParams{
			Controller: req.Controller, APIKey: req.APIKey,
			Servers: servers, SelfIP: servers[0].LocalIP,
		}, progress)
		if err != nil {
			return // already recorded on progress
		}
		if err := a.store.SetServerware(cfg); err != nil {
			progress.Fail(err)
			return
		}
		progress.Finish()
	}()
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

func (a *app) handleServerwareStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	a.mu.Lock()
	progress, ok := a.serverwareJobs[jobID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown job_id", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, progress.Snapshot())
}

// handleServerwareRestart restarts PBXware instances' VPSs through
// SERVERware, one at a time, waiting for each PBXware to come back up (to
// apply a recording RAM disk raised while connecting SERVERware). Body:
// {"server_ids": [...]}. Progress is read like the connect job's.
func (a *app) handleServerwareRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ServerIDs []string `json:"server_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if a.testRunning() {
		writeError(w, http.StatusConflict, errors.New("a test run is in progress; restart the VPSs after it finishes"))
		return
	}
	sw := a.store.Serverware()
	if sw == nil {
		writeError(w, http.StatusBadRequest, errors.New("connect SERVERware first"))
		return
	}
	var targets []wizard.RestartTarget
	for _, id := range req.ServerIDs {
		srv := a.store.GetServer(id)
		ref, ok := sw.PBXwareVPS[id]
		if srv == nil || !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unknown PBXware instance %q; reconnect SERVERware", id))
			return
		}
		targets = append(targets, wizard.RestartTarget{Server: srv, VPS: ref})
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no instances to restart"))
		return
	}

	progress := &wizard.ServerwareProgress{}
	jobID := a.newJobID()
	a.mu.Lock()
	a.serverwareJobs[jobID] = progress
	a.mu.Unlock()
	client := serverware.NewClient(sw.ControllerURL, sw.APIKey)
	go func() { _ = wizard.RestartPBXware(client, targets, progress) }()
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

// handleServerware reports the saved SERVERware connection, without any
// keys.
func (a *app) handleServerware(w http.ResponseWriter, r *http.Request) {
	sw := a.store.Serverware()
	if sw == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	vpsNames := []string{sw.SwarmDialerVPS.Name}
	for _, v := range sw.PBXwareVPS {
		vpsNames = append(vpsNames, v.Name)
	}
	dt := a.store.DTCollector()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":              true,
		"controller_url":          sw.ControllerURL,
		"host_name":               sw.HostName,
		"vps_names":               vpsNames,
		"dt_collector_url":        dtcollector.URLOrDefault(dt.URL),
		"dt_collector_configured": dt.Key != "",
	})
}

// saveDTCollector checks an upload key against DT Collector (url empty =
// the default) and saves it. An empty key keeps the saved one, so the URL
// can be changed without re-entering the key.
func (a *app) saveDTCollector(url, key string) error {
	cur := a.store.DTCollector()
	if key == "" {
		key = cur.Key
	}
	if key == "" {
		return errors.New("enter the DT Collector upload key")
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == dtcollector.DefaultURL {
		url = "" // follow the default if it ever changes
	}
	if err := dtcollector.NewClient(dtcollector.URLOrDefault(url), key).Ping(); err != nil {
		return fmt.Errorf("checking the upload key at %s: %w", dtcollector.URLOrDefault(url), err)
	}
	return a.store.SetDTCollector(store.DTCollector{URL: url, Key: strings.TrimSpace(key)})
}

// handleDTCollector reads (GET, without the key) or saves (POST) the DT
// Collector settings.
func (a *app) handleDTCollector(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			URL string `json:"url"`
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.saveDTCollector(req.URL, req.Key); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	dt := a.store.DTCollector()
	writeJSON(w, http.StatusOK, map[string]any{
		"url": dtcollector.URLOrDefault(dt.URL), "default_url": dtcollector.DefaultURL,
		"overridden": dt.URL != "", "key_set": dt.Key != "",
	})
}
