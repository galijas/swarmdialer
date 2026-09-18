// Package wizard implements the Setup Wizard's backend logic: connecting
// to a PBXware instance, provisioning a tenant + extensions on it (with
// live progress), and connecting two servers with a trunk + one DID per
// extension. See docs/gui_spec.md for the full spec this implements.
package wizard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/netutil"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// SIPHostFrom derives the SIP signaling target (host:port) from a PBXware
// base URL like "https://10.1.100.208" — same host, standard SIP port.
// PBXware's HTTP API and SIP signaling share the same host in every setup
// we've tested; there's no separate field for this in the wizard spec.
func SIPHostFrom(baseURL string) string {
	host := baseURL
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(host, prefix) {
			host = host[len(prefix):]
			break
		}
	}
	host = strings.TrimSuffix(host, "/")
	return host + ":5060"
}

// ConnectionResult is what TestConnection returns on success.
type ConnectionResult struct {
	License pbxware.LicenseInfo
	LocalIP string // auto-detected — see netutil.DetectLocalIP
}

// TestConnection validates a PBXware base URL + API key by making a
// lightweight read call (license.info) and detects the local IP to
// advertise for SIP on this connection. PBXware API keys appear to be
// all-or-nothing admin keys (no separate read/write scoping observed), so
// a successful read is treated as sufficient confirmation of write access
// too — see docs/gui_spec.md's open-question note on this.
func TestConnection(baseURL, apiKey string) (ConnectionResult, error) {
	client := pbxware.NewClient(baseURL, apiKey)
	license, err := client.GetLicenseInfo()
	if err != nil {
		return ConnectionResult{}, fmt.Errorf("connecting to %s: %w", baseURL, err)
	}
	localIP, err := netutil.DetectLocalIP(SIPHostFrom(baseURL))
	if err != nil {
		return ConnectionResult{}, fmt.Errorf("detecting local IP for %s: %w", baseURL, err)
	}
	return ConnectionResult{License: license, LocalIP: localIP}, nil
}

// ProvisionProgress reports live progress of a provisioning run — read it
// concurrently (e.g. from an HTTP handler) via Snapshot while Provision
// runs in the background.
type ProvisionProgress struct {
	mu      sync.Mutex
	total   int
	created int
	message string
	done    bool
	err     string
}

func (p *ProvisionProgress) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// ProvisionProgressSnapshot is a point-in-time, serializable copy of a
// ProvisionProgress — deliberately a separate type (not a copy of
// ProvisionProgress itself), since that struct embeds a sync.Mutex and
// copying it is a real bug (caught by `go vet`'s copylocks check).
type ProvisionProgressSnapshot struct {
	Total   int    `json:"total"`
	Created int    `json:"created"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Err     string `json:"error,omitempty"`
}

// Snapshot returns a copy safe to read or serialize without locking.
func (p *ProvisionProgress) Snapshot() ProvisionProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProvisionProgressSnapshot{Total: p.total, Created: p.created, Message: p.message, Done: p.done, Err: p.err}
}

// ProvisionParams configures one server's provisioning step.
type ProvisionParams struct {
	Name           string // display name for this server
	BaseURL        string
	APIKey         string
	ExtensionCount int
	// Tenant creation fields — ignored (and tenant creation skipped) if
	// the license's edition isn't Multi-Tenant.
	TenantCode    string
	TenantName    string
	ExtLength     int
	Package       string
	Country       string
	National      string
	International string
	ChannelLimit  int // raised from PBXware's 8 default — see pbxware.SetTenantChannelLimits
	MaxWait       time.Duration
}

// Provision creates (or, for non-Multi-Tenant editions, skips) a tenant
// and ExtensionCount extensions on it, reporting progress via progress
// (pass a fresh &ProvisionProgress{} and poll Snapshot from elsewhere
// while this runs — it blocks until done or failed). Returns a
// store.Server ready to be saved.
func Provision(ctx context.Context, p ProvisionParams, progress *ProvisionProgress) (*store.Server, error) {
	progress.set(func() { progress.total = p.ExtensionCount; progress.message = "connecting" })

	client := pbxware.NewClient(p.BaseURL, p.APIKey)

	license, err := client.GetLicenseInfo()
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("checking license: %w", err)
	}

	localIP, err := netutil.DetectLocalIP(SIPHostFrom(p.BaseURL))
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("detecting local IP: %w", err)
	}

	srv := &store.Server{
		ID:      fmt.Sprintf("srv-%d", time.Now().UnixNano()),
		Name:    p.Name,
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Edition: license.Edition,
		SIPHost: SIPHostFrom(p.BaseURL),
		LocalIP: localIP,
	}

	tenantID := 1 // system level — used directly for non-Multi-Tenant editions
	if license.IsMultiTenant() {
		progress.set(func() { progress.message = "creating tenant" })
		id, err := client.AddTenant(pbxware.TenantParams{
			Name:          p.TenantName,
			Code:          p.TenantCode,
			Package:       p.Package,
			ExtLength:     p.ExtLength,
			Country:       p.Country,
			National:      p.National,
			International: p.International,
		})
		if err != nil {
			// tenant.add frequently outlasts the HTTP timeout even when it
			// succeeds — check tenant.list before giving up (same pattern
			// as cmd/swarmdialer).
			tenants, listErr := client.ListTenants()
			if listErr == nil {
				for _, t := range tenants {
					if t.Code == p.TenantCode {
						id = t.ID
						err = nil
						break
					}
				}
			}
			if err != nil {
				progress.set(func() { progress.done = true; progress.err = err.Error() })
				return nil, fmt.Errorf("creating tenant: %w", err)
			}
		}
		if err := client.WaitForTenant(id, p.MaxWait); err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("tenant %d never showed up: %w", id, err)
		}

		progress.set(func() { progress.message = "raising tenant channel limit" })
		if err := client.SetTenantChannelLimits(id, p.ChannelLimit, p.MaxWait); err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("raising channel limit: %w", err)
		}

		tenantID = id
		srv.TenantID = id
		srv.TenantCode = p.TenantCode
	} else {
		progress.set(func() { progress.message = "non-Multi-Tenant edition — provisioning extensions at system level" })
	}

	extensions := make([]pbxware.ProvisionedExtension, 0, p.ExtensionCount)
	for i := 0; i < p.ExtensionCount; i++ {
		if ctx.Err() != nil {
			progress.set(func() { progress.done = true; progress.err = ctx.Err().Error() })
			return nil, ctx.Err()
		}
		extNum := 100 + i
		secret, err := pbxware.GenerateSecret()
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("generating secret: %w", err)
		}

		result, err := client.AddExtensionWithRetry(pbxware.ExtensionParams{
			Server:        tenantID,
			Name:          fmt.Sprintf("SwarmDialer %d", extNum),
			Email:         fmt.Sprintf("swarmdialer%d@swarmdialer.local", extNum),
			Ext:           strconv.Itoa(extNum),
			Secret:        secret,
			UA:            50,             // Generic SIP
			IncomingLimit: p.ChannelLimit, // was hardcoded to 2 — capped every extension's own concurrency, not just the tenant's
			OutgoingLimit: p.ChannelLimit,
			AllowedCodecs: "ulaw:alaw",
		}, p.MaxWait)
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("creating extension %d: %w", extNum, err)
		}

		cfg, err := client.GetExtensionConfig(tenantID, result.ExtensionID)
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("fetching config for extension %d: %w", extNum, err)
		}

		extensions = append(extensions, pbxware.ProvisionedExtension{
			ExtensionID: result.ExtensionID,
			Ext:         cfg.Ext,
			Username:    cfg.Username,
			Secret:      cfg.Secret,
		})
		progress.set(func() {
			progress.created = i + 1
			progress.message = fmt.Sprintf("created extension %s (%d/%d)", cfg.Ext, i+1, p.ExtensionCount)
		})
	}

	srv.Extensions = extensions
	progress.set(func() { progress.done = true; progress.message = "done" })
	return srv, nil
}
