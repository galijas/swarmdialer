package main

import (
	"encoding/json"
	"net/http"

	"swarmdialer/internal/pbxware"
)

// recordingTarget resolves which v2 client + tenant/system id a dialer
// section's recording controls act on — same "always the caller/local
// side" rule as batchReportTarget, since recording (like MOS/CDR) is a
// property of whichever instance actually handles the call's leg, not the
// callee's. edition is returned too since the stereo-recording field is
// only ever system-level (see pbxware.SystemTenantID) — callers that also
// touch stereo need to know whether tenantID here is a real Multi-Tenant
// tenant (in which case stereo needs its own separate PATCH to
// SystemTenantID) or already SystemTenantID itself.
func (a *app) recordingTarget(section, serverID, peerServerID string) (client *pbxware.ClientV2, tenantID int, edition string, err error) {
	srv := a.store.GetServer(serverID)
	if srv == nil {
		return nil, 0, "", errServerNotFound
	}
	if section == "remote" {
		if a.store.GetServer(peerServerID) == nil {
			return nil, 0, "", errServerNotFound
		}
	}
	tenantID = srv.TenantID
	if !pbxware.IsMultiTenantEdition(srv.Edition) {
		tenantID = pbxware.SystemTenantID
	}
	return pbxware.NewClientV2(srv.BaseURL, srv.APIKeyV2), tenantID, srv.Edition, nil
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
func readRecordingStatus(client *pbxware.ClientV2, tenantID int, edition string) (recordingStatusResponse, error) {
	cfg, err := client.GetTenant(tenantID)
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

// handleRecordingStatus reads a dialer section's current recording state
// (?section=local|remote&server_id=...&peer_server_id=...) — used to
// initialize the recording row's controls when the server/pair selection
// changes, so the toggle reflects PBXware's actual current setting rather
// than always starting from an assumed "off".
func (a *app) handleRecordingStatus(w http.ResponseWriter, r *http.Request) {
	client, tenantID, edition, err := a.recordingTarget(
		r.URL.Query().Get("section"), r.URL.Query().Get("server_id"), r.URL.Query().Get("peer_server_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	status, err := readRecordingStatus(client, tenantID, edition)
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
	client, tenantID, edition, err := a.recordingTarget(section, serverID, peerServerID)
	if err != nil {
		return recordingStatusResponse{}, false
	}
	status, err := readRecordingStatus(client, tenantID, edition)
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
	Format       string `json:"format"` // recording codec: gsm/wav/wav49/g729/ogg
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// handleRecordingToggle sets a dialer section's recording state via API
// v2. enabled/format are PATCHed onto the target tenant (a real tenant ID
// for Multi-Tenant, or SystemTenantID for non-Multi-Tenant — see
// recordingTarget). stereo_recording_enabled is always system-level
// regardless of edition (confirmed live 2026-09-24 — see
// pbxware.SystemTenantID's doc comment), so on Multi-Tenant it needs its
// own separate PATCH to SystemTenantID; on non-Multi-Tenant that's already
// the same id as the first PATCH, so the second call is a harmless repeat
// of the same target.
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

	client, tenantID, edition, err := a.recordingTarget(req.Section, req.ServerID, req.PeerServerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	cfg, err := client.PatchTenant(tenantID, map[string]any{
		"call_recordings": map[string]any{
			"enabled": yesNo(req.Enabled),
			"format":  req.Format,
		},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	stereoTenantID := tenantID
	if pbxware.IsMultiTenantEdition(edition) {
		stereoTenantID = pbxware.SystemTenantID
	}
	stereoCfg, err := client.PatchTenant(stereoTenantID, map[string]any{
		"call_recordings": map[string]any{
			"stereo_recording_enabled": yesNo(req.Stereo),
		},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	// Report back what PBXware actually confirms is set, not just what was
	// requested — the two PATCH responses above are the source of truth
	// (see the RAM-disk dependency this uncovered live 2026-09-24: a stereo
	// PATCH can silently no-op instead of erroring if the target isn't
	// actually able to honor it yet).
	writeJSON(w, http.StatusOK, recordingStatusResponse{
		Enabled:       cfg.CallRecordings.Enabled == "yes",
		StereoEnabled: stereoCfg.CallRecordings.StereoRecordingEnabled == "yes",
		Format:        cfg.CallRecordings.Format,
	})
}
