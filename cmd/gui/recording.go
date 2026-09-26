package main

import (
	"encoding/json"
	"net/http"

	"swarmdialer/internal/pbxware"
)

// recordingTarget is one PBXware instance a dialer section's recording
// controls act on: its v2 client, the tenant/system id recording is set on
// (a real tenant on Multi-Tenant, SystemTenantID otherwise), and its
// edition — needed because stereo recording is only ever system-level (see
// pbxware.SystemTenantID), so on Multi-Tenant it takes its own PATCH.
type recordingTarget struct {
	client   *pbxware.ClientV2
	tenantID int
	edition  string
}

// recordingTargets resolves every instance a dialer section's recording
// controls act on. Local: just the server. Remote: both servers of the
// pair — a remote call runs through both PBXware instances, and each one
// records only if recording is on for it (confirmed live 2026-09-26: with
// only the caller side toggled, the peer recorded nothing). The caller
// side comes first.
func (a *app) recordingTargets(section, serverID, peerServerID string) ([]recordingTarget, error) {
	ids := []string{serverID}
	if section == "remote" {
		ids = append(ids, peerServerID)
	}
	targets := make([]recordingTarget, 0, len(ids))
	for _, id := range ids {
		srv := a.store.GetServer(id)
		if srv == nil {
			return nil, errServerNotFound
		}
		tenantID := srv.TenantID
		if !pbxware.IsMultiTenantEdition(srv.Edition) {
			tenantID = pbxware.SystemTenantID
		}
		targets = append(targets, recordingTarget{
			client:   pbxware.NewClientV2(srv.BaseURL, srv.APIKeyV2),
			tenantID: tenantID,
			edition:  srv.Edition,
		})
	}
	return targets, nil
}

// recordingStatusResponse is the current recording state for one dialer
// section's target — shown when the section/server selection changes and
// echoed back after a successful toggle.
type recordingStatusResponse struct {
	Enabled       bool   `json:"enabled"`
	StereoEnabled bool   `json:"stereo_enabled"`
	Format        string `json:"format"`
}

// readRecordingStatus reads a target's current recording state — shared by
// handleRecordingStatus (the dashboard's recording row) and
// currentRecordingStatus (batch logging, see batchreport.go), so both
// agree on the same MT-stereo-split handling.
//
// stereo_recording_enabled is always system-level (see
// pbxware.SystemTenantID) — on Multi-Tenant, tenantID is a real tenant,
// whose own record omits that field entirely (decodes to ""), so it needs
// its own read from SystemTenantID, mirroring handleRecordingToggle's
// write-side split.
func readRecordingStatus(t recordingTarget) (recordingStatusResponse, error) {
	client, edition := t.client, t.edition
	cfg, err := client.GetTenant(t.tenantID)
	if err != nil {
		return recordingStatusResponse{}, err
	}

	stereoEnabled := cfg.CallRecordings.StereoRecordingEnabled == "yes"
	if pbxware.IsMultiTenantEdition(edition) {
		systemCfg, err := client.GetTenant(pbxware.SystemTenantID)
		if err != nil {
			return recordingStatusResponse{}, err
		}
		stereoEnabled = systemCfg.CallRecordings.StereoRecordingEnabled == "yes"
	}

	return recordingStatusResponse{
		Enabled:       cfg.CallRecordings.Enabled == "yes",
		StereoEnabled: stereoEnabled,
		Format:        cfg.CallRecordings.Format,
	}, nil
}

// readCombinedRecordingStatus reads every target's state and reports
// recording (and stereo) as on only if it's on for all of them, so a
// half-enabled remote pair shows as off and one toggle turns both on. The
// format shown is the caller side's.
func readCombinedRecordingStatus(targets []recordingTarget) (recordingStatusResponse, error) {
	var combined recordingStatusResponse
	for i, t := range targets {
		st, err := readRecordingStatus(t)
		if err != nil {
			return recordingStatusResponse{}, err
		}
		if i == 0 {
			combined = st
			continue
		}
		combined.Enabled = combined.Enabled && st.Enabled
		combined.StereoEnabled = combined.StereoEnabled && st.StereoEnabled
	}
	return combined, nil
}

// handleRecordingStatus reads a dialer section's current recording state
// (?section=local|remote&server_id=...&peer_server_id=...) — used to
// initialize the recording row's controls when the server/pair selection
// changes, so the toggle reflects PBXware's actual current setting rather
// than always starting from an assumed "off".
func (a *app) handleRecordingStatus(w http.ResponseWriter, r *http.Request) {
	targets, err := a.recordingTargets(
		r.URL.Query().Get("section"), r.URL.Query().Get("server_id"), r.URL.Query().Get("peer_server_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	status, err := readCombinedRecordingStatus(targets)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// currentRecordingStatus is readRecordingStatus for batch-logging purposes
// (see batchreport.go) — best-effort: on any failure to resolve the target
// or read its state, it returns a zero-value "unknown" status rather than
// failing the batch over a logging detail.
func (a *app) currentRecordingStatus(section, serverID, peerServerID string) (recordingStatusResponse, bool) {
	targets, err := a.recordingTargets(section, serverID, peerServerID)
	if err != nil {
		return recordingStatusResponse{}, false
	}
	status, err := readCombinedRecordingStatus(targets)
	if err != nil {
		return recordingStatusResponse{}, false
	}
	return status, true
}

type recordingToggleRequest struct {
	Section      string `json:"section"`
	ServerID     string `json:"server_id"`
	PeerServerID string `json:"peer_server_id"`
	Enabled      bool   `json:"enabled"`
	Stereo       bool   `json:"stereo"`
	Format       string `json:"format"` // recording codec: gsm/wav/wav49/ogg
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// handleRecordingToggle sets a dialer section's recording state via API v2
// on every target (see recordingTargets — both instances for remote).
func (a *app) handleRecordingToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req recordingToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !req.Enabled {
		req.Stereo = false // stereo never makes sense with recording itself off
	}

	targets, err := a.recordingTargets(req.Section, req.ServerID, req.PeerServerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	statuses := make([]recordingStatusResponse, 0, len(targets))
	for _, t := range targets {
		st, err := applyRecordingSettings(t, req)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		statuses = append(statuses, st)
	}

	// Report back what PBXware actually confirms is set on every target,
	// combined the same way as readCombinedRecordingStatus.
	combined := statuses[0]
	for _, st := range statuses[1:] {
		combined.Enabled = combined.Enabled && st.Enabled
		combined.StereoEnabled = combined.StereoEnabled && st.StereoEnabled
	}
	writeJSON(w, http.StatusOK, combined)
}

// applyRecordingSettings PATCHes one target's recording settings and
// returns what PBXware confirms is now set. enabled/format go on the
// target's tenant (or system) record; stereo_recording_enabled always goes
// on the system record (see pbxware.SystemTenantID) — on non-Multi-Tenant
// that's the same record, so the second PATCH is a harmless repeat.
func applyRecordingSettings(t recordingTarget, req recordingToggleRequest) (recordingStatusResponse, error) {
	cfg, err := t.client.PatchTenant(t.tenantID, map[string]any{
		"call_recordings": map[string]any{
			"enabled": yesNo(req.Enabled),
			"format":  req.Format,
		},
	})
	if err != nil {
		return recordingStatusResponse{}, err
	}

	stereoTenantID := t.tenantID
	if pbxware.IsMultiTenantEdition(t.edition) {
		stereoTenantID = pbxware.SystemTenantID
	}
	stereoCfg, err := t.client.PatchTenant(stereoTenantID, map[string]any{
		"call_recordings": map[string]any{
			"stereo_recording_enabled": yesNo(req.Stereo),
		},
	})
	if err != nil {
		return recordingStatusResponse{}, err
	}

	// The PATCH responses are the source of truth, not the request: a
	// stereo PATCH can silently no-op instead of erroring if the target
	// can't honor it yet (the RAM-disk dependency found live 2026-09-24).
	return recordingStatusResponse{
		Enabled:       cfg.CallRecordings.Enabled == "yes",
		StereoEnabled: stereoCfg.CallRecordings.StereoRecordingEnabled == "yes",
		Format:        cfg.CallRecordings.Format,
	}, nil
}
