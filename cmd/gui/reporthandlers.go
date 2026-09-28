package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
