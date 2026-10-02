package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/dtcollector"
	"swarmdialer/internal/report"
	"swarmdialer/internal/testrun"
)

// reportsDir keeps a local copy of every report (no secrets).
const reportsDir = "reports"

// uploadRetries are the waits between upload attempts after the first.
var uploadRetries = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// reportState is the last run's report and its upload, for the
// Monitoring tab.
type reportState struct {
	ReportID  string    `json:"report_id,omitempty"`
	Profile   string    `json:"profile,omitempty"`
	State     string    `json:"state"` // none | saved | uploading | uploaded | failed
	Message   string    `json:"message,omitempty"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
	path      string
}

type reportTracker struct {
	mu    sync.Mutex
	state reportState
	rep   *report.Report
}

func (t *reportTracker) set(mutate func(*reportState)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mutate(&t.state)
	t.state.UpdatedAt = time.Now()
	if t.state.ReportID != "" {
		writeUploadStatus(t.state)
	}
}

// uploadStatus is kept next to each saved report (<id>.upload.json), so
// the Previous Reports list knows whether it was uploaded, across
// restarts.
type uploadStatus struct {
	State     string    `json:"state"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func uploadStatusPath(id string) string { return filepath.Join(reportsDir, id+".upload.json") }

func writeUploadStatus(st reportState) {
	data, err := json.Marshal(uploadStatus{State: st.State, Message: st.Message, UpdatedAt: st.UpdatedAt})
	if err == nil {
		_ = os.WriteFile(uploadStatusPath(st.ReportID), data, 0o600)
	}
}

func (t *reportTracker) get() reportState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// finishRun builds the report for a finished run, saves it locally, and
// (for a complete run of an uploadable profile) uploads it.
func (a *app) finishRun(runner *testrun.Runner, profile testrun.Profile, in report.Inputs) {
	st := runner.Status()
	results := runner.Results()
	complete := st.Error == "" && len(results) == len(profile.Tests)
	for _, r := range results {
		if r.StopReason == testrun.StopCancelled || r.StopReason == testrun.StopError {
			complete = false
		}
	}
	if !complete {
		a.reports.set(func(s *reportState) {
			*s = reportState{State: "none", Profile: profile.Name, Message: "the run was cancelled or a test failed, so no report was made"}
		})
		return
	}

	rep, err := report.Build(in, results)
	if err != nil {
		a.reports.set(func(s *reportState) {
			*s = reportState{State: "failed", Profile: profile.Name, Message: "building the report: " + err.Error()}
		})
		return
	}
	path, err := saveReport(rep)
	if err != nil {
		a.reports.set(func(s *reportState) {
			*s = reportState{State: "failed", Profile: profile.Name, Message: "saving the report: " + err.Error()}
		})
		return
	}
	a.reports.mu.Lock()
	a.reports.rep = rep
	a.reports.state = reportState{ReportID: rep.ReportID, Profile: profile.Name, State: "saved", path: path, UpdatedAt: time.Now(), Message: "saved locally as " + path}
	a.reports.mu.Unlock()

	if profile.Name != testrun.Standard.Name {
		a.reports.set(func(s *reportState) {
			s.Message = fmt.Sprintf("saved locally as %s. %s runs aren't uploaded (only the standard profile is comparable)", path, profile.Name)
		})
		return
	}
	a.uploadReport()
}

func saveReport(rep *report.Report) (string, error) {
	if err := os.MkdirAll(reportsDir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(reportsDir, rep.ReportID+".json")
	return path, os.WriteFile(path, data, 0o600)
}

// uploadReport uploads the current report, retrying transient failures.
// Runs in the calling goroutine.
func (a *app) uploadReport() {
	a.reports.mu.Lock()
	rep := a.reports.rep
	a.reports.mu.Unlock()
	if rep == nil {
		return
	}
	dt := a.store.DTCollector()
	if dt.Key == "" {
		a.reports.set(func(s *reportState) {
			s.State, s.Message = "saved", "saved locally; not uploaded because no DT Collector upload key is set (Settings), then use Retry upload"
		})
		return
	}
	client := dtcollector.NewClient(dtcollector.URLOrDefault(dt.URL), dt.Key)
	for attempt := 0; ; attempt++ {
		a.reports.set(func(s *reportState) {
			s.State, s.Attempts, s.Message = "uploading", s.Attempts+1, "uploading to "+client.BaseURL
		})
		res, err := client.Upload(rep)
		if err == nil {
			msg := "uploaded to " + client.BaseURL
			if res.Duplicate {
				msg += " (DT Collector already had it)"
			}
			a.reports.set(func(s *reportState) { s.State, s.Message = "uploaded", msg })
			log.Printf("report %s %s", rep.ReportID, msg)
			return
		}
		var perm *dtcollector.PermanentError
		if errors.As(err, &perm) || attempt >= len(uploadRetries) {
			a.reports.set(func(s *reportState) { s.State, s.Message = "failed", err.Error() })
			log.Printf("report %s upload failed: %v", rep.ReportID, err)
			return
		}
		wait := uploadRetries[attempt]
		a.reports.set(func(s *reportState) {
			s.State, s.Message = "uploading", fmt.Sprintf("upload failed (%v); retrying in %s", err, wait)
		})
		time.Sleep(wait)
	}
}

func (a *app) handleReportStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.reports.get())
}

// handleReportUpload retries the current report's upload now (e.g. after
// setting the upload key, or after it failed).
func (a *app) handleReportUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	st := a.reports.get()
	switch {
	case st.ReportID == "":
		writeError(w, http.StatusBadRequest, errors.New("there's no report to upload"))
		return
	case st.State == "uploading" || st.State == "uploaded":
		writeError(w, http.StatusConflict, fmt.Errorf("the report is already %s", st.State))
		return
	case st.Profile != testrun.Standard.Name:
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s runs aren't uploaded", st.Profile))
		return
	}
	go a.uploadReport()
	writeJSON(w, http.StatusOK, map[string]string{"status": "uploading"})
}

// handleFinishWipe deletes SwarmDialer's configuration, and with it every
// stored key and credential (PBXware and SERVERware API keys, the DT
// Collector upload key, extension SIP secrets), then restarts. Offered
// after a run's report is uploaded; never automatic. The admin login, call
// logs, run results and reports (which hold no secrets) are kept, and
// nothing on PBXware or SERVERware is changed.
func (a *app) handleFinishWipe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if a.testRunning() {
		writeError(w, http.StatusConflict, errors.New("a test run is in progress"))
		return
	}
	if st := a.reports.get(); st.State == "uploading" {
		writeError(w, http.StatusConflict, errors.New("the report is still uploading"))
		return
	}
	if err := os.Remove(a.configPath); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "wiped, restarting"})
	go func() {
		time.Sleep(500 * time.Millisecond) // let the response go out
		restartProcess()
	}()
}

// reportIDPattern guards the file-based report endpoints against path
// traversal: report IDs are UUIDs.
var reportIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func loadReport(id string) (*report.Report, []byte, error) {
	if !reportIDPattern.MatchString(id) {
		return nil, nil, errors.New("invalid report id")
	}
	data, err := os.ReadFile(filepath.Join(reportsDir, id+".json"))
	if err != nil {
		return nil, nil, err
	}
	var rep report.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, nil, err
	}
	return &rep, data, nil
}

func readUploadStatus(id string) uploadStatus {
	var st uploadStatus
	if data, err := os.ReadFile(uploadStatusPath(id)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// handleListReports lists saved reports, newest first.
func (a *app) handleListReports(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID          string    `json:"id"`
		Profile     string    `json:"profile"`
		CreatedAt   time.Time `json:"created_at"`
		CPUModel    string    `json:"cpu_model"`
		Tests       int       `json:"tests"`
		SizeBytes   int64     `json:"size_bytes"`
		UploadState string    `json:"upload_state"`
	}
	entries, err := os.ReadDir(reportsDir)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []item{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".upload.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		rep, _, err := loadReport(id)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		it := item{ID: id, Profile: fmt.Sprintf("%s v%d", rep.Profile.Name, rep.Profile.Version), CreatedAt: rep.CreatedAt,
			CPUModel: rep.Environment.Host.CPUModel, Tests: len(rep.Tests), UploadState: readUploadStatus(id).State}
		if info != nil {
			it.SizeBytes = info.Size()
		}
		if it.UploadState == "" {
			it.UploadState = "saved"
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

// handleViewReport renders a saved report as readable text.
func (a *app) handleViewReport(w http.ResponseWriter, r *http.Request) {
	rep, _, err := loadReport(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, reportText(rep, readUploadStatus(rep.ReportID)))
}

// handleDownloadReport sends a saved report's JSON as a file.
func (a *app) handleDownloadReport(w http.ResponseWriter, r *http.Request) {
	rep, data, err := loadReport(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="swarmdialer-report-%s-%s.json"`, rep.CreatedAt.UTC().Format("20060102-1504"), rep.ReportID[:8]))
	_, _ = w.Write(data)
}

// handleDeleteReport deletes a saved report (only the local copy; one
// already uploaded stays in DT Collector).
func (a *app) handleDeleteReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !reportIDPattern.MatchString(req.ID) {
		writeError(w, http.StatusBadRequest, errors.New("invalid report id"))
		return
	}
	if st := a.reports.get(); st.ReportID == req.ID && st.State == "uploading" {
		writeError(w, http.StatusConflict, errors.New("that report is still uploading"))
		return
	}
	if err := os.Remove(filepath.Join(reportsDir, req.ID+".json")); err != nil {
		writeError(w, http.StatusNotFound, errors.New("report not found"))
		return
	}
	_ = os.Remove(uploadStatusPath(req.ID))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// reportText is a readable rendering of a report.
func reportText(rep *report.Report, up uploadStatus) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	f1 := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	e, h := rep.Environment, rep.Environment.Host

	p("SwarmDialer SW Host Benchmark report")
	p("Report:   %s", rep.ReportID)
	p("Created:  %s", rep.CreatedAt.Local().Format("2006-01-02 15:04:05 MST"))
	p("Profile:  %s v%d   (SwarmDialer %s, report format v%d)", rep.Profile.Name, rep.Profile.Version, rep.SwarmDialerVersion, rep.SchemaVersion)
	upload := up.State
	if upload == "" {
		upload = "saved locally"
	}
	if up.Message != "" {
		upload += ": " + up.Message
	}
	p("Upload:   %s", upload)
	p("")
	p("=== Host ===")
	p("CPU:      %s (%d sockets, %d cores, %d threads, max %d MHz)", h.CPUModel, h.CPUSockets, h.CPUCores, h.CPUThreads, h.CPUMaxMHz)
	p("Memory:   %s GB", f1(float64(h.MemoryBytes)/(1<<30)))
	if h.SystemVendor != "" || h.SystemModel != "" {
		p("System:   %s %s", h.SystemVendor, h.SystemModel)
	}
	if mb := h.Motherboard; mb != nil {
		line := strings.TrimSpace(mb.Vendor + " " + mb.Model)
		if mb.Version != "" {
			line += " v" + mb.Version
		}
		if mb.BIOSVersion != "" {
			line += fmt.Sprintf(", BIOS %s %s (%s)", mb.BIOSVendor, mb.BIOSVersion, mb.BIOSDate)
		}
		p("Board:    %s", line)
	}
	for _, d := range h.Disks {
		p("Disk:     %s (%s)", d.Model, d.Type)
	}
	count := func(n int) string {
		if n > 1 {
			return fmt.Sprintf(" ×%d", n)
		}
		return ""
	}
	for i, c := range h.StorageControllers {
		p("Storage controller %d: %s %s%s", i+1, c.Vendor, c.Product, count(c.Count))
	}
	for i, n := range h.NICs {
		line := fmt.Sprintf("NIC %d: %s %s", i+1, n.Vendor, n.Product)
		if n.SpeedMbps > 0 {
			line += fmt.Sprintf(", %s link", mbpsText(n.SpeedMbps))
		}
		if n.Driver != "" {
			line += " (" + n.Driver + ")"
		}
		p("%s%s", line, count(n.Count))
	}
	for i, b := range h.Bonds {
		p("Bond %d:   %d ports bonded", i+1, b.Ports)
	}
	for _, n := range h.Network {
		p("Link:     %s", mbpsText(n.SpeedMbps))
	}
	p("SERVERware %s, %s edition", e.Serverware.Version, e.Serverware.Edition)
	for _, x := range e.PBXware {
		p("PBXware %s: %s, %s, %d-channel license", x.Role, x.Version, x.Edition, x.LicenseChannels)
	}
	for _, name := range []string{"pbxware_mt", "pbxware_cc", "swarmdialer"} {
		if v, ok := e.VPS[name]; ok {
			cpu := "unlimited CPU"
			if v.CPULimit > 0 {
				cpu = fmt.Sprintf("%d-core CPU limit", v.CPULimit)
			}
			line := fmt.Sprintf("VPS %s: %s, %d MB memory", name, cpu, v.MemLimitMB)
			if v.CallrecRAMMB > 0 {
				line += fmt.Sprintf(", %d MB recording RAM disk", v.CallrecRAMMB)
			}
			p("%s", line)
		}
	}
	if len(rep.Tests) == 0 {
		p("")
		p("Hardware info only: no tests were run.")
	}
	for i, t := range rep.Tests {
		res := t.Result
		p("")
		p("=== Test %d: %s ===", i+1, t.ID)
		rec := t.Recording
		if t.RecordingFormat != "" {
			rec += " (" + t.RecordingFormat + ")"
		}
		p("Setup:    %s, codec %s -> %s, recording %s, %ds calls at up to %s calls/s", t.Mode, t.Codec.Caller, t.Codec.Callee, rec, t.CallDurationS, strconv.FormatFloat(t.DialRateCPS, 'f', -1, 64))
		p("Time:     %s to %s", t.StartedAt.Local().Format("15:04:05"), t.FinishedAt.Local().Format("15:04:05"))
		p("Result:   %s, max %d calls at once", res.StopReason, res.MaxConcurrentCalls)
		if res.StopDetail != "" {
			p("Stopped:  %s", res.StopDetail)
		}
		if a := res.AtStop; a != nil {
			line := fmt.Sprintf("At stop:  %d calls, host CPU %s%%, memory %s%%, I/O wait %s%%", a.Calls, f1(a.HostCPUPct), f1(a.HostMemPct), f1(a.HostIOWaitPct))
			for _, role := range []string{"MT", "CC"} {
				v, ok := a.VPS[role]
				if !ok {
					continue
				}
				line += fmt.Sprintf("; %s CPU %s%% of host", role, f1(v.CPUPctOfHost))
				if v.CPUPctOfLimit != nil {
					line += fmt.Sprintf(" (%s%% of its limit)", f1(*v.CPUPctOfLimit))
				}
				if v.MemPctOfLimit != nil {
					line += fmt.Sprintf(", memory %s%% of its limit", f1(*v.MemPctOfLimit))
				}
			}
			p("%s", line)
		}
		if len(res.Failures) > 0 {
			var parts []string
			for _, f := range res.Failures {
				part := f.Cause
				if f.SIPCode != 0 {
					part += fmt.Sprintf(" %d", f.SIPCode)
				}
				part += fmt.Sprintf(" x%d", f.Count)
				if f.FirstAtCalls != nil {
					part += fmt.Sprintf(" (from %d calls)", *f.FirstAtCalls)
				}
				parts = append(parts, part)
			}
			p("Failures: %s", strings.Join(parts, ", "))
		}
		p("Calls:    %d started, %d answered, %d failed", res.Calls.Started, res.Calls.Answered, res.Calls.Failed)
		p("Setup ms: avg %s, p95 %s, max %s", f1(res.SetupMS.Avg), f1(res.SetupMS.P95), f1(res.SetupMS.Max))
		p("MOS:      avg %s, min %s", f1(res.MOS.Avg), f1(res.MOS.Min))
		p("RTP:      %s%% received", f1(res.RTPReceivedRatio*100))
		if res.QualityDegradedAtCalls != nil {
			why := ""
			if res.QualityDegradedReason != "" {
				why = " (" + res.QualityDegradedReason + ")"
			}
			p("Quality:  first dropped at %d calls%s", *res.QualityDegradedAtCalls, why)
		}
		if pv := res.PBXware; pv != nil {
			for _, role := range []string{"MT", "CC"} {
				var parts []string
				for st, n := range pv.CDRStatus[role] {
					parts = append(parts, fmt.Sprintf("%s %d", st, n))
				}
				sort.Strings(parts)
				line := fmt.Sprintf("PBXware:  %s CDRs: %s", role, strings.Join(parts, ", "))
				if peak, ok := pv.ActiveCallsPeak[role]; ok {
					line += fmt.Sprintf("; peak %s active calls (PBXware's count)", f1(peak))
				}
				p("%s", line)
			}
		}
		if tl := res.Tool; tl != nil {
			line := fmt.Sprintf("Tool:     SwarmDialer CPU peak %s%%, UDP drops send %d / receive %d, extensions not registered %d -> %d",
				f1(tl.SwarmDialerCPUPeakPct), tl.UDPSendErrors, tl.UDPReceiveErrors, tl.NotRegisteredAtStart, tl.NotRegisteredAtEnd)
			for _, role := range []string{"MT", "CC"} {
				if m, ok := tl.MediaReceived[role]; ok {
					line += fmt.Sprintf("; audio from %s: %s%% lost, jitter avg %s ms", role, f1(m.LossPct), f1(m.JitterMSAvg))
				}
			}
			p("%s", line)
		}
		if n := len(res.Events); n > 0 {
			p("Events:   %d (see the report JSON)", n)
		}
		ast := res.AtTarget.AsteriskCPUPct
		p("At load:  host CPU %s%%, host memory %s%%, Asterisk CPU MT %s%% / CC %s%% of a core", f1(res.AtTarget.HostCPUPct), f1(res.AtTarget.HostMemPct), f1(ast["MT"]), f1(ast["CC"]))
		if r := res.Recording; r != nil {
			if r.RAMDiskFullEstimatedAtCalls != nil {
				p("RAM disk: estimated full at %d calls", *r.RAMDiskFullEstimatedAtCalls)
			}
			d := r.MP3ConversionDelayS
			p("MP3:      conversion delay avg %ss, p95 %ss, max %ss (%s)", f1(d.Avg), f1(d.P95), f1(d.Max), d.Trend)
		}
		peakCPU, peakCalls := 0.0, 0
		for _, v := range t.Timeseries.Series.HostCPUPct {
			peakCPU = max(peakCPU, v)
		}
		for _, v := range t.Timeseries.Series.ConcurrentCalls {
			peakCalls = max(peakCalls, v)
		}
		p("Samples:  %d every %ds (peak host CPU %s%%, peak %d calls)", len(t.Timeseries.Series.HostCPUPct), t.Timeseries.IntervalS, f1(peakCPU), peakCalls)
	}
	return b.String()
}

func mbpsText(mbps int) string {
	if mbps >= 1000 && mbps%1000 == 0 {
		return fmt.Sprintf("%d Gbit/s", mbps/1000)
	}
	return fmt.Sprintf("%d Mbit/s", mbps)
}

// hwReportMu stops overlapping hardware-only uploads (a double click).
var hwReportMu sync.Mutex

// handleHardwareReport builds a hardware-only report (see
// testrun.Hardware: the host's hardware and environment, no tests), saves
// it locally, and uploads it to DT Collector once, straight away.
func (a *app) handleHardwareReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !hwReportMu.TryLock() {
		writeError(w, http.StatusConflict, errors.New("a hardware report is already being sent"))
		return
	}
	defer hwReportMu.Unlock()

	env, err := a.gatherEnvironment(testrun.Hardware)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	rep, err := report.Build(env.in, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("building the hardware report: %w", err))
		return
	}
	path, err := saveReport(rep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("saving the hardware report: %w", err))
		return
	}
	st := reportState{ReportID: rep.ReportID, Profile: testrun.Hardware.Name, UpdatedAt: time.Now()}
	dt := a.store.DTCollector()
	if dt.Key == "" {
		st.State, st.Message = "saved", "saved locally as "+path+"; not uploaded because no DT Collector upload key is set (Settings)"
	} else {
		client := dtcollector.NewClient(dtcollector.URLOrDefault(dt.URL), dt.Key)
		if res, err := client.Upload(rep); err != nil {
			st.State, st.Message = "failed", err.Error()
		} else {
			st.State, st.Message = "uploaded", "uploaded to "+client.BaseURL
			if res.Duplicate {
				st.Message += " (DT Collector already had it)"
			}
		}
	}
	writeUploadStatus(st)
	writeJSON(w, http.StatusOK, map[string]string{"report_id": rep.ReportID, "state": st.State, "message": st.Message})
}
