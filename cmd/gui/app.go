package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"swarmdialer/internal/logstore"
	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/statusapi"
	"swarmdialer/internal/store"
	"swarmdialer/internal/wizard"
)

//go:embed web
var webFS embed.FS

// app holds all server-side state for the GUI: the persisted server
// config, in-flight wizard jobs, and the dashboard's call sessions.
//
// Sessions are keyed explicitly by which server(s) they dial, not by a
// fixed "the local one" / "the remote one" slot — with more than one
// server configured (e.g. two tenants on the same PBXware box), there can
// be a separate local session per server and a separate remote session
// per connected pair, all live at once. See handlers.go's sessionFor.
type app struct {
	ctx        context.Context
	store      *store.Store
	logs       *logstore.Store
	configPath string // see handleResetSwarmDialer

	mu             sync.Mutex
	provisionJobs  map[string]*wizard.ProvisionProgress
	connectJobs    map[string]*wizard.ConnectProgress
	resetJobs      map[string]*wizard.ResetProgress
	localSessions  map[string]*orchestrator.Session // server ID -> session
	remoteSessions map[string]*orchestrator.Session // "callerID|calleeID" -> session
	nextJobID      uint64
	nextPortOffset int // grows so successive sessions don't reuse ports
}

func newApp(ctx context.Context, configPath string) (*app, error) {
	st, err := store.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	logs, err := logstore.New("logs")
	if err != nil {
		return nil, fmt.Errorf("opening log store: %w", err)
	}
	return &app{
		ctx:            ctx,
		store:          st,
		logs:           logs,
		configPath:     configPath,
		provisionJobs:  make(map[string]*wizard.ProvisionProgress),
		connectJobs:    make(map[string]*wizard.ConnectProgress),
		resetJobs:      make(map[string]*wizard.ResetProgress),
		localSessions:  make(map[string]*orchestrator.Session),
		remoteSessions: make(map[string]*orchestrator.Session),
	}, nil
}

func (a *app) newJobID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextJobID++
	return fmt.Sprintf("job-%d", a.nextJobID)
}

// allocPortRange reserves the next n local SIP ports for a new session,
// so a local session and a later remote session (or a rebuilt session
// after reconfiguration) never collide on the same ports.
func (a *app) allocPortRange(n int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	base := 20000 + a.nextPortOffset
	a.nextPortOffset += n + 10 // small gap for safety
	return base
}

func (a *app) registerRoutes(mux *http.ServeMux) {
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // embed misconfiguration — programmer error, not runtime
	}
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	mux.HandleFunc("/api/wizard/test-connection", a.handleTestConnection)
	mux.HandleFunc("/api/wizard/verify-system-settings", a.handleVerifySystemSettings)
	mux.HandleFunc("/api/wizard/provision", a.handleProvision)
	mux.HandleFunc("/api/wizard/provision/status", a.handleProvisionStatus)
	mux.HandleFunc("/api/wizard/connect", a.handleConnect)
	mux.HandleFunc("/api/wizard/connect/status", a.handleConnectStatus)
	mux.HandleFunc("/api/servers", a.handleServers)
	mux.HandleFunc("/api/dial", a.handleDial)
	mux.HandleFunc("/api/stop", a.handleStop)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/settings/reset-instance", a.handleResetInstance)
	mux.HandleFunc("/api/settings/reset-instance/status", a.handleResetInstanceStatus)
	mux.HandleFunc("/api/settings/reset-swarmdialer", a.handleResetSwarmDialer)
	mux.HandleFunc("/api/settings/clear-logs", a.handleClearLogs)
	mux.HandleFunc("/api/logs", a.handleListLogs)
	mux.HandleFunc("/api/logs/view", a.handleViewLog)
	mux.HandleFunc("/api/logs/download", a.handleDownloadLog)
	mux.HandleFunc("/api/logs/delete", a.handleDeleteLog)
	mux.Handle("/ws/status", a.sessionWSHandler())
}

// restartProcess replaces the current process image with a fresh
// invocation of itself (same binary, same args, same environment) —
// used by handleResetSwarmDialer so "reset to default" actually clears
// in-memory state (sessions, port allocations, job maps) rather than just
// the on-disk config, without needing a supervisor (systemd, etc.) to
// bounce the process from outside. Go's listener sockets are opened
// close-on-exec, so the old port-80 listener is released as part of the
// exec itself — the new process binds it again immediately.
func restartProcess() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("restart: resolving executable path: %v", err)
		return
	}
	args := append([]string{exe}, os.Args[1:]...)
	if err := syscall.Exec(exe, args, os.Environ()); err != nil {
		log.Printf("restart: exec failed: %v", err)
	}
}

func (a *app) sessionWSHandler() http.Handler {
	return statusapi.WSHandler(func() any {
		return map[string]any{"sessions": a.allSnapshots()}
	}, time.Second)
}

// namedSnapshot tags one live session's snapshot with which server(s) it
// belongs to, so the dashboard can show every session at once (not just
// "the" local one and "the" remote one) and label/color them correctly.
type namedSnapshot struct {
	Key      string                       `json:"key"`
	Type     string                       `json:"type"` // "local" or "remote"
	Label    string                       `json:"label"`
	Snapshot orchestrator.SessionSnapshot `json:"snapshot"`
}

func (a *app) allSnapshots() []namedSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]namedSnapshot, 0, len(a.localSessions)+len(a.remoteSessions))
	for id, sess := range a.localSessions {
		label := id
		if srv := a.store.GetServer(id); srv != nil {
			label = srv.Name
		}
		out = append(out, namedSnapshot{Key: "local:" + id, Type: "local", Label: label, Snapshot: sess.Snapshot()})
	}
	for key, sess := range a.remoteSessions {
		callerID, calleeID, _ := strings.Cut(key, "|")
		label := key
		if caller, callee := a.store.GetServer(callerID), a.store.GetServer(calleeID); caller != nil && callee != nil {
			label = caller.Name + " → " + callee.Name
		}
		out = append(out, namedSnapshot{Key: "remote:" + key, Type: "remote", Label: label, Snapshot: sess.Snapshot()})
	}
	return out
}
