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
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Edition string `json:"edition"`

	// SIPHost is the actual network destination for SIP (host:port,
	// usually the same host as BaseURL on port 5060). LocalIP is our own
	// address advertised in Contact headers when registering extensions
	// on this server — auto-detected, see detectLocalIP.
	SIPHost string `json:"sip_host"`
	LocalIP string `json:"local_ip"`

	TenantID   int    `json:"tenant_id"`   // 0 if this edition skips tenant creation
	TenantCode string `json:"tenant_code"`

	Extensions []pbxware.ProvisionedExtension `json:"extensions"`

	// Trunk/DID fields are only populated once a second server is added.
	TrunkID      int          `json:"trunk_id,omitempty"`
	PeerServerID string       `json:"peer_server_id,omitempty"`
	DIDs         []DIDMapping `json:"dids,omitempty"`
}

// Config is the full persisted state.
type Config struct {
	Servers []*Server `json:"servers"`
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
