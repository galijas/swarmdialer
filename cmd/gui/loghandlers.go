package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"swarmdialer/internal/logstore"
)

var errInvalidSection = errors.New(`section must be "local" or "remote"`)

func sectionParam(r *http.Request) (logstore.Section, error) {
	return parseSection(r.URL.Query().Get("section"))
}

func parseSection(s string) (logstore.Section, error) {
	switch s {
	case "local":
		return logstore.SectionLocal, nil
	case "remote":
		return logstore.SectionRemote, nil
	default:
		return "", errInvalidSection
	}
}

// handleListLogs lists every batch log for one dialer section (?section=local|remote).
func (a *app) handleListLogs(w http.ResponseWriter, r *http.Request) {
	section, err := sectionParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	logs, err := a.logs.List(section)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// handleViewLog returns one log file's content as plain text
// (?section=local|remote&name=<file>).
func (a *app) handleViewLog(w http.ResponseWriter, r *http.Request) {
	section, err := sectionParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := r.URL.Query().Get("name")
	data, err := a.logs.Read(section, name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

// handleDownloadLog is the same content as handleViewLog, but with headers
// that make the browser save it as a file instead of displaying it.
func (a *app) handleDownloadLog(w http.ResponseWriter, r *http.Request) {
	section, err := sectionParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := r.URL.Query().Get("name")
	data, err := a.logs.Read(section, name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(data)
}

// handleDeleteLog deletes one log file.
func (a *app) handleDeleteLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Section string `json:"section"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	section, err := parseSection(req.Section)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.logs.Delete(section, req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// handleClearLogs deletes every log file, both sections — Settings tab's
// "Clear All Logs" button.
func (a *app) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := a.logs.DeleteAll(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
