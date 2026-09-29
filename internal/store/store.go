// Package store persists SwarmDialer's configuration — which PBXware
// servers are connected, what was provisioned on each, and (for a second
// server) the trunk/DID mapping between them — to a local JSON file, so
// the GUI survives a process restart.
//
// This is per-deployment state, not part of the product itself: a fresh
// deployment on a new VPS starts with no file and an empty config, and the
// Setup Wizard populates it from scratch. Never ship a pre-populated file.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"swarmdialer/internal/pbxware"
)

// DIDMapping is one DID number routed to one extension on this server, for
// remote-calling: the peer server dials this DID over the trunk to reach
// the mapped extension.
type DIDMapping struct {
	DID string `json:"did"`
	Ext string `json:"ext"`
}

// Server is one connected PBXware instance and everything provisioned on
// it so far.
type Server struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"` // legacy (v1) API key
	APIKeyV2 string `json:"api_key_v2"`
	Edition  string `json:"edition"`

	// SIPHost is the actual network destination for SIP (host:port,
	// usually the same host as BaseURL on port 5060). LocalIP is our own
	// address advertised in Contact headers when registering extensions
	// on this server — auto-detected, see detectLocalIP.
	SIPHost string `json:"sip_host"`
	LocalIP string `json:"local_ip"`

	TenantID   int    `json:"tenant_id"` // 0 if this edition skips tenant creation
	TenantCode string `json:"tenant_code"`

	Extensions []pbxware.ProvisionedExtension `json:"extensions"`

	// Trunk/DID fields are only populated once a second server is added.
	TrunkID      int          `json:"trunk_id,omitempty"`
	PeerServerID string       `json:"peer_server_id,omitempty"`
	DIDs         []DIDMapping `json:"dids,omitempty"`
}

// VPSRef identifies one VPS on the SERVERware controller.
type VPSRef struct {
	ID   int    `json:"id"`
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// Serverware is the SERVERware site the connected PBXware instances (and
// SwarmDialer itself) run on, used for host monitoring during the test
// script. All three VPSs must be on the same host (HostID).
type Serverware struct {
	ControllerURL string `json:"controller_url"`
	APIKey        string `json:"api_key"`

	HostID   int    `json:"host_id"`
	HostUUID string `json:"host_uuid"`
	HostName string `json:"host_name"`

	// PBXwareVPS maps a connected server's ID (Server.ID) to its VPS.
	PBXwareVPS     map[string]VPSRef `json:"pbxware_vps"`
	SwarmDialerVPS VPSRef            `json:"swarmdialer_vps"`
}

// DTCollector is where finished test reports are uploaded. URL empty means
// the built-in default (see dtcollector.DefaultURL).
type DTCollector struct {
	URL string `json:"url,omitempty"`
	Key string `json:"key,omitempty"`
}

// Config is the full persisted state.
type Config struct {
	Servers     []*Server    `json:"servers"`
	Serverware  *Serverware  `json:"serverware,omitempty"`
	DTCollector *DTCollector `json:"dt_collector,omitempty"`
	// WizardCompleted records that the Setup Wizard was finished, which
	// hides its tab. nil (configs from before this field) counts as
	// finished if any server is configured.
	WizardCompleted *bool `json:"wizard_completed,omitempty"`
}

// Store guards Config with a mutex and persists it to path on every
// mutation.
type Store struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

// Open loads path if it exists, or starts with an empty Config if not
// (the normal case for a fresh deployment).
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	return os.Rename(tmp, s.path)
}

// Servers returns a snapshot of all configured servers.
func (s *Store) Servers() []*Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Server, len(s.cfg.Servers))
	copy(out, s.cfg.Servers)
	return out
}

// GetServer returns one server by ID, or nil if not found.
func (s *Store) GetServer(id string) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.cfg.Servers {
		if srv.ID == id {
			return srv
		}
	}
	return nil
}

// AddServer appends a new server and persists.
func (s *Store) AddServer(srv *Server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Servers = append(s.cfg.Servers, srv)
	return s.save()
}

// UpdateServer replaces the server with the same ID (mutate a copy from
// GetServer, then call this) and persists.
func (s *Store) UpdateServer(srv *Server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.cfg.Servers {
		if existing.ID == srv.ID {
			s.cfg.Servers[i] = srv
			return s.save()
		}
	}
	return fmt.Errorf("no server with id %q", srv.ID)
}

// Serverware returns a copy of the SERVERware connection, or nil if none.
func (s *Store) Serverware() *Serverware {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Serverware == nil {
		return nil
	}
	cp := *s.cfg.Serverware
	return &cp
}

// SetServerware saves (or, with nil, removes) the SERVERware connection.
func (s *Store) SetServerware(sw *Serverware) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Serverware = sw
	return s.save()
}

// DTCollector returns a copy of the DT Collector settings (never nil).
func (s *Store) DTCollector() DTCollector {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.DTCollector == nil {
		return DTCollector{}
	}
	return *s.cfg.DTCollector
}

// SetDTCollector saves the DT Collector settings.
func (s *Store) SetDTCollector(dt DTCollector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.DTCollector = &dt
	return s.save()
}

// WizardCompleted reports whether the Setup Wizard is finished. It never is
// while no server is configured (e.g. after every instance was reset), so
// the wizard comes back when there's nothing left to use.
func (s *Store) WizardCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cfg.Servers) == 0 {
		return false
	}
	return s.cfg.WizardCompleted == nil || *s.cfg.WizardCompleted
}

// SetWizardCompleted records whether the Setup Wizard is finished.
func (s *Store) SetWizardCompleted(done bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.WizardCompleted = &done
	return s.save()
}

// RemoveServer deletes the server with the given ID and persists — used by
// Settings' "reset instance" (see wizard.ResetInstance for what's torn
// down on PBXware itself before this is called).
func (s *Store) RemoveServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.cfg.Servers {
		if existing.ID == id {
			s.cfg.Servers = append(s.cfg.Servers[:i], s.cfg.Servers[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("no server with id %q", id)
}
