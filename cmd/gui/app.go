package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/statusapi"
	"swarmdialer/internal/store"
	"swarmdialer/internal/wizard"
)

//go:embed web
var webFS embed.FS

// app holds all server-side state for the GUI: the persisted server
// config, in-flight wizard jobs, and the two dashboard call sessions
// (local/remote), created lazily on first use.
type app struct {
	ctx   context.Context
	store *store.Store

	mu             sync.Mutex
	provisionJobs  map[string]*wizard.ProvisionProgress
	connectJobs    map[string]*wizard.ConnectProgress
	localSession   *orchestrator.Session
	remoteSession  *orchestrator.Session
	nextJobID      uint64
	nextPortOffset int // grows so successive sessions don't reuse ports
}

func newApp(ctx context.Context, configPath string) (*app, error) {
	st, err := store.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	return &app{
		ctx:           ctx,
		store:         st,
		provisionJobs: make(map[string]*wizard.ProvisionProgress),
		connectJobs:   make(map[string]*wizard.ConnectProgress),
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
	mux.HandleFunc("/api/wizard/provision", a.handleProvision)
	mux.HandleFunc("/api/wizard/provision/status", a.handleProvisionStatus)
	mux.HandleFunc("/api/wizard/connect", a.handleConnect)
	mux.HandleFunc("/api/wizard/connect/status", a.handleConnectStatus)
	mux.HandleFunc("/api/servers", a.handleServers)
	mux.HandleFunc("/api/dial", a.handleDial)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.Handle("/ws/status", a.sessionWSHandler())
}

func (a *app) sessionWSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		section := r.URL.Query().Get("section")
		getSession := func() *orchestrator.Session {
			a.mu.Lock()
			defer a.mu.Unlock()
			if section == "remote" {
				return a.remoteSession
			}
			return a.localSession
		}
		statusapi.SessionWSHandler(getSession, time.Second).ServeHTTP(w, r)
	})
}
