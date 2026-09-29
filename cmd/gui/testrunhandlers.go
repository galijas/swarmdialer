package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"swarmdialer/internal/dtcollector"
	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/report"
	"swarmdialer/internal/serverware"
	"swarmdialer/internal/store"
	"swarmdialer/internal/testrun"
)

// runsDir holds each finished test run's full results (no secrets).
const runsDir = "runs"

// testPair returns the MT (caller) and CC (callee) servers: the test
// script's calls always go from the Multi-Tenant instance to the other.
func (a *app) testPair() (mt, cc *store.Server, err error) {
	for _, s := range a.store.Servers() {
		if pbxware.IsMultiTenantEdition(s.Edition) {
			mt = s
		} else {
			cc = s
		}
	}
	if mt == nil || cc == nil {
		return nil, nil, errors.New("the benchmark needs two PBXware instances connected by a trunk; currently one must be Multi-Tenant (calls are placed from it) and the other not")
	}
	if mt.PeerServerID != cc.ID {
		return nil, nil, errors.New("the two PBXware instances aren't connected with a trunk yet (Setup Wizard step 6)")
	}
	return mt, cc, nil
}

// testEnv is testrun.Env backed by the app.
type testEnv struct {
	a      *app
	mt, cc *store.Server
}

func (e *testEnv) RemoteSession() (*orchestrator.Session, error) {
	return e.a.sessionFor("remote", e.mt.ID, e.cc.ID)
}

func (e *testEnv) SetRecording(mode, format string) error {
	targets, err := e.a.recordingTargets("remote", e.mt.ID, e.cc.ID)
	if err != nil {
		return err
	}
	req := recordingToggleRequest{Enabled: mode != testrun.RecordingOff, Stereo: mode == testrun.RecordingStereo, Format: format}
	for _, t := range targets {
		if _, err := applyRecordingSettings(t, req); err != nil {
			return err
		}
	}
	return nil
}

func (e *testEnv) PrepareTrunk(codec string) (func(), error) {
	note, release, err := e.a.prepareRemoteBatch(e.mt, e.cc, codec)
	if note != "" {
		log.Printf("testrun: %s", note)
	}
	return release, err
}

func (e *testEnv) RecordingCDRs(since time.Time) (map[string][]pbxware.RecordingCDR, error) {
	out := map[string][]pbxware.RecordingCDR{}
	for role, s := range map[string]*store.Server{"MT": e.mt, "CC": e.cc} {
		serverID := s.TenantID
		if serverID == 0 {
			serverID = 1 // non-Multi-Tenant: CDRs live at system level
		}
		list, err := pbxware.NewClient(s.BaseURL, s.APIKey).RecordingCDRsSince(serverID, since)
		if err != nil {
			return out, fmt.Errorf("%s: %w", role, err)
		}
		out[role] = list
	}
	return out, nil
}

func (e *testEnv) CallMOS(records []orchestrator.CallRecord) (testrun.MOS, error) {
	serverID := e.mt.TenantID // the calling side's CDRs
	if serverID == 0 {
		serverID = 1
	}
	windows := make([]pbxware.CallWindow, len(records))
	for i, c := range records {
		windows[i] = pbxware.CallWindow{Ext: c.CallerAOR, Start: c.StartedAt, End: c.EndedAt}
	}
	byWindow := pbxware.NewClient(e.mt.BaseURL, e.mt.APIKey).BatchGetMOS(serverID, windows)
	var m testrun.MOS
	sum := 0.0
	for _, v := range byWindow {
		if v.Avg <= 0 {
			continue
		}
		if m.N == 0 || v.Min < m.Min {
			m.Min = v.Min
		}
		sum += v.Avg
		m.N++
	}
	if m.N > 0 {
		m.Avg = math.Round(100*sum/float64(m.N)) / 100
	}
	return m, nil
}

type testRunStartRequest struct {
	Profile string `json:"profile"`
}

// handleTestRunStart starts the test script (see internal/testrun) in the
// background. Only one run at a time; manual dialing is refused while it
// runs (see handleDial), so nothing else disturbs the measurements.
func (a *app) handleTestRunStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req testRunStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	profile, ok := testrun.Profiles[req.Profile]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown profile %q", req.Profile))
		return
	}
	a.mu.Lock()
	if a.testRunner != nil && a.testRunner.Status().Running {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("a test run is already in progress"))
		return
	}
	a.mu.Unlock()

	env, err := a.gatherEnvironment(profile)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	in, mon, limits, mt, cc := env.in, env.mon, env.limits, env.mt, env.cc

	runner := testrun.NewRunner(&testEnv{a: a, mt: mt, cc: cc}, mon, limits, profile)
	a.mu.Lock()
	a.testRunner = runner
	a.mu.Unlock()
	a.reports.set(func(s *reportState) {
		*s = reportState{State: "none", Profile: profile.Name, Message: "the report is made when the run finishes"}
	})
	go func() {
		runner.Run()
		if err := saveRun(runner); err != nil {
			log.Printf("testrun: saving results: %v", err)
		}
		a.finishRun(runner, profile, in)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// saveRun writes a finished run's full results to runsDir.
func saveRun(r *testrun.Runner) error {
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return err
	}
	st := r.Status()
	data, err := json.MarshalIndent(map[string]any{
		"profile": st.Profile, "started_at": st.StartedAt, "error": st.Error,
		"results": r.Results(), "log": st.Log,
	}, "", " ")
	if err != nil {
		return err
	}
	name := filepath.Join(runsDir, st.StartedAt.UTC().Format("20060102-150405")+".json")
	return os.WriteFile(name, data, 0o600)
}

func (a *app) currentRunner() *testrun.Runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.testRunner
}

func (a *app) testRunning() bool {
	r := a.currentRunner()
	return r != nil && r.Status().Running
}

func (a *app) handleTestRunStatus(w http.ResponseWriter, r *http.Request) {
	runner := a.currentRunner()
	if runner == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false})
		return
	}
	writeJSON(w, http.StatusOK, runner.Status())
}

func (a *app) handleTestRunCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if runner := a.currentRunner(); runner != nil {
		runner.Cancel()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}

// handleTestRunResults returns the current (or last) run's full results,
// including every sample.
func (a *app) handleTestRunResults(w http.ResponseWriter, r *http.Request) {
	runner := a.currentRunner()
	if runner == nil {
		writeError(w, http.StatusNotFound, errors.New("no test run yet"))
		return
	}
	writeJSON(w, http.StatusOK, runner.Results())
}

// handleTestRunProfiles lists the available profiles with their tests and
// estimated duration, for the Monitoring tab.
func (a *app) handleTestRunProfiles(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Name        string         `json:"name"`
		Version     int            `json:"version"`
		EstimateMin int            `json:"estimate_min"`
		Tests       []testrun.Test `json:"tests"`
	}
	out := []view{}
	for _, name := range []string{testrun.Standard.Name, testrun.Smoke.Name} {
		p := testrun.Profiles[name]
		out = append(out, view{Name: p.Name, Version: p.Version, EstimateMin: int(p.EstimatedDuration().Minutes() + 0.5), Tests: p.Tests})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTestRunReadiness reports what's needed before the test script can
// run, for the Monitoring tab's checklist.
func (a *app) handleTestRunReadiness(w http.ResponseWriter, r *http.Request) {
	type check struct {
		Label string `json:"label"`
		OK    bool   `json:"ok"`
		Warn  bool   `json:"warn,omitempty"` // not required yet
		Note  string `json:"note,omitempty"`
	}
	var checks []check
	_, _, pairErr := a.testPair()
	note := ""
	if pairErr != nil {
		note = pairErr.Error()
	}
	checks = append(checks, check{Label: "Two PBXware instances connected by a trunk", OK: pairErr == nil, Note: note})
	sw := a.store.Serverware()
	if sw == nil {
		checks = append(checks, check{Label: "SERVERware connected", Note: "connect it in Settings or wizard step 7"})
	} else {
		checks = append(checks, check{Label: "SERVERware connected", OK: true, Note: fmt.Sprintf("%s, host %s", sw.ControllerURL, sw.HostName)})
	}
	dt := a.store.DTCollector()
	if dt.Key != "" {
		checks = append(checks, check{Label: "DT Collector upload key set", OK: true, Note: "reports upload to " + dtcollector.URLOrDefault(dt.URL)})
	} else {
		checks = append(checks, check{Label: "DT Collector upload key set", Warn: true, Note: "set it in Settings, or the finished report is only saved locally"})
	}
	ready := true
	for _, c := range checks {
		if !c.OK && !c.Warn {
			ready = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": ready, "checks": checks})
}

// httpError is an error with the HTTP status to answer it with.
type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return http.StatusInternalServerError
}

// environment is what a report (and a test run) needs to know about the
// tested host and instances.
type environment struct {
	in     report.Inputs
	mon    *testrun.Monitor
	limits map[string]testrun.VPSLimits
	mt, cc *store.Server
}

// gatherEnvironment checks the setup and reads everything a report's
// environment section needs: the tested host's hardware and active node,
// the PBXware VPSs' limits, versions and licenses, and SERVERware's
// edition. Shared by the test script and "Upload Hardware Info Only".
func (a *app) gatherEnvironment(profile testrun.Profile) (*environment, error) {
	bad := func(status int, format string, args ...any) (*environment, error) {
		return nil, &httpError{status, fmt.Errorf(format, args...)}
	}
	swCfg := a.store.Serverware()
	if swCfg == nil {
		return bad(http.StatusBadRequest, "connect SERVERware first (Setup Wizard step 7, or Settings)")
	}
	mt, cc, err := a.testPair()
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, err}
	}
	sw := serverware.NewClient(swCfg.ControllerURL, swCfg.APIKey)
	vpsNames := map[string]string{}
	limits := map[string]testrun.VPSLimits{}
	in := report.Inputs{Profile: profile, Serverware: sw, VPS: map[string]report.VPS{}}
	for role, s := range map[string]*store.Server{"MT": mt, "CC": cc} {
		ref, ok := swCfg.PBXwareVPS[s.ID]
		if !ok {
			return bad(http.StatusBadRequest, "%s's VPS isn't known; reconnect SERVERware (Settings)", s.Name)
		}
		v, err := sw.GetVPS(ref.ID)
		if err != nil {
			return bad(http.StatusBadGateway, "reading %s's VPS from SERVERware: %v", s.Name, err)
		}
		if v.HostID != swCfg.HostID {
			return bad(http.StatusBadRequest, "%s's VPS has moved to another host; all VPSs must be on host %s. Reconnect SERVERware after moving it back", s.Name, swCfg.HostName)
		}
		vpsNames[role] = ref.Name
		limits[role] = testrun.VPSLimits{CPULimit: v.CPULimit, MemLimitMB: v.MemLimitMB, CallrecRAMMB: v.CallrecRAMMB}
		in.VPS["pbxware_"+strings.ToLower(role)] = report.VPS{CPULimit: v.CPULimit, CPUShare: v.CPUShare, MemLimitMB: v.MemLimitMB, CallrecRAMMB: v.CallrecRAMMB}
		lic, err := pbxware.NewClient(s.BaseURL, s.APIKey).GetLicenseInfo()
		if err != nil {
			return bad(http.StatusBadGateway, "reading %s's license: %v", s.Name, err)
		}
		in.PBXware = append(in.PBXware, report.PBXwareInstance{Role: role, Version: strings.TrimSpace(v.PBXwareVersion), Edition: s.Edition, LicenseChannels: lic.Channels})
	}
	if v, err := sw.GetVPS(swCfg.SwarmDialerVPS.ID); err == nil {
		in.VPS["swarmdialer"] = report.VPS{CPULimit: v.CPULimit, MemLimitMB: v.MemLimitMB}
	}
	hosts, err := sw.Hosts()
	if err != nil {
		return bad(http.StatusBadGateway, "reading SERVERware hosts: %v", err)
	}
	in.Edition = report.Edition(hosts)
	for _, h := range hosts {
		if h.ID == swCfg.HostID {
			in.Platform = h.PlatformDetails
		}
	}
	if in.Platform.CPUModel == "" {
		return bad(http.StatusBadGateway, "SERVERware doesn't report host %s's CPU model (host platform details); reports need it to be comparable", swCfg.HostName)
	}
	sort.Slice(in.PBXware, func(i, j int) bool { return in.PBXware[i].Role > in.PBXware[j].Role }) // MT, CC
	mon, err := testrun.NewMonitor(sw, swCfg.HostName, vpsNames)
	if err != nil {
		return nil, &httpError{http.StatusBadGateway, err}
	}
	in.Node = mon.Node()
	return &environment{in: in, mon: mon, limits: limits, mt: mt, cc: cc}, nil
}
