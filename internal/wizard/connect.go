package wizard

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// ConnectProgress reports live progress of trunk+DID creation between two
// servers.
type ConnectProgress struct {
	mu      sync.Mutex
	stage   string // "trunk", "default-trunk", "dids", "settling"
	total   int
	created int
	done    bool
	err     string
}

func (p *ConnectProgress) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// ConnectProgressSnapshot is a point-in-time, serializable copy of a
// ConnectProgress (kept separate from ConnectProgress itself since that
// struct embeds a sync.Mutex — see ProvisionProgressSnapshot's doc
// comment in wizard.go for why).
type ConnectProgressSnapshot struct {
	Stage   string `json:"stage"`
	Total   int    `json:"total"`
	Created int    `json:"created"`
	Done    bool   `json:"done"`
	Err     string `json:"error,omitempty"`
}

// Snapshot returns a copy safe to read or serialize without locking.
func (p *ConnectProgress) Snapshot() ConnectProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ConnectProgressSnapshot{Stage: p.stage, Total: p.total, Created: p.created, Done: p.done, Err: p.err}
}

// ConnectServers creates a SIP trunk between two already-provisioned
// servers (symmetric: a trunk on each side, with matching credentials so
// they authenticate each other) and one DID per extension on *each* side,
// mapped back to that extension — so either server's extensions can dial
// the other's via the trunk. For any Multi-Tenant side, also sets the new
// trunk as that tenant's default outbound trunk (see
// pbxware.SetTenantDefaultTrunk — required for outbound calls to actually
// establish, not just optional). Mutates a and b in place (TrunkID, DIDs)
// — save them via the caller's store after this returns.
//
// See docs/pbxware_api_reference.md's Trunks/DIDs sections: trunks are
// system-level objects (server=1) even in Tenant Mode.
func ConnectServers(a, b *store.Server, progress *ConnectProgress) error {
	progress.set(func() { progress.stage = "trunk" })

	clientA := pbxware.NewClient(a.BaseURL, a.APIKey)
	clientB := pbxware.NewClient(b.BaseURL, b.APIKey)

	credA, err := pbxware.GenerateSecret()
	if err != nil {
		return fmt.Errorf("generating credentials: %w", err)
	}
	credB, err := pbxware.GenerateSecret()
	if err != nil {
		return fmt.Errorf("generating credentials: %w", err)
	}
	userA, userB := "swarm"+a.ID, "swarm"+b.ID

	providerIDA, err := clientA.GenericSIPProviderID(1)
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return fmt.Errorf("finding Generic SIP provider on %s: %w", a.Name, err)
	}
	providerIDB, err := clientB.GenericSIPProviderID(1)
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return fmt.Errorf("finding Generic SIP provider on %s: %w", b.Name, err)
	}

	aHost := hostOnly(a.SIPHost)
	bHost := hostOnly(b.SIPHost)

	trunkA, err := clientA.AddTrunk(pbxware.TrunkParams{
		Server: 1, Name: "SwarmDialer-to-" + b.Name, ProviderID: providerIDA,
		Host: aHost, Username: userA, Secret: credA,
		PeerHost: bHost, PeerUsername: userB, PeerSecret: credB,
		Country: "869", National: "1", International: "011",
	})
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return fmt.Errorf("creating trunk on %s: %w", a.Name, err)
	}

	trunkB, err := clientB.AddTrunk(pbxware.TrunkParams{
		Server: 1, Name: "SwarmDialer-to-" + a.Name, ProviderID: providerIDB,
		Host: bHost, Username: userB, Secret: credB,
		PeerHost: aHost, PeerUsername: userA, PeerSecret: credA,
		Country: "869", National: "1", International: "011",
	})
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return fmt.Errorf("creating trunk on %s: %w", b.Name, err)
	}

	a.TrunkID, b.TrunkID = trunkA.TrunkID, trunkB.TrunkID
	a.PeerServerID, b.PeerServerID = b.ID, a.ID

	progress.set(func() { progress.stage = "default-trunk" })
	if isMultiTenantEdition(a.Edition) {
		if err := clientA.SetTenantDefaultTrunk(a.TenantID, trunkA.TrunkID); err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return fmt.Errorf("setting default trunk on %s: %w", a.Name, err)
		}
	}
	if isMultiTenantEdition(b.Edition) {
		if err := clientB.SetTenantDefaultTrunk(b.TenantID, trunkB.TrunkID); err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return fmt.Errorf("setting default trunk on %s: %w", b.Name, err)
		}
	}

	total := len(a.Extensions) + len(b.Extensions)
	progress.set(func() { progress.stage = "dids"; progress.total = total })

	// DID numbers must not collide within a PBXware instance (both sides
	// might be tenants on the *same* instance during testing) — prefix
	// with the tenant code to keep each side's numbers distinct.
	if err := addDIDsFor(clientA, a, trunkA.TrunkID, progress); err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return err
	}
	if err := addDIDsFor(clientB, b, trunkB.TrunkID, progress); err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return err
	}

	// Trunks (like tenants and extensions) need a settling period after
	// creation before they're actually usable for routing — confirmed
	// live: dialing a freshly-created DID through a freshly-created trunk
	// failed with "No such extension/context ... while calling Local
	// channel" immediately after creation, then succeeded once ~90s had
	// passed with no other change. There's no read-back call that
	// confirms "this trunk is ready" the way tenant.configuration does for
	// channel limits, so this is a blind wait rather than retry+verify —
	// tuned empirically, may need adjustment on a different/faster
	// PBXware instance.
	progress.set(func() { progress.stage = "settling" })
	time.Sleep(90 * time.Second)

	progress.set(func() { progress.done = true })
	return nil
}

func addDIDsFor(client *pbxware.Client, srv *store.Server, trunkID int, progress *ConnectProgress) error {
	tenantID := srv.TenantID
	if tenantID == 0 {
		tenantID = 1
	}
	dids := make([]store.DIDMapping, 0, len(srv.Extensions))
	for _, ext := range srv.Extensions {
		did := fmt.Sprintf("9%s%s", srv.TenantCode, ext.Ext)
		if _, err := client.AddDID(pbxware.DIDParams{
			Server: tenantID, TrunkID: trunkID, DID: did, Destination: ext.Ext,
		}); err != nil {
			return fmt.Errorf("creating DID %s for extension %s on %s: %w", did, ext.Ext, srv.Name, err)
		}
		dids = append(dids, store.DIDMapping{DID: did, Ext: ext.Ext})
		progress.set(func() { progress.created++ })
	}
	srv.DIDs = dids
	return nil
}

func isMultiTenantEdition(edition string) bool {
	return strings.EqualFold(edition, "Multi-Tenant")
}

func hostOnly(sipHost string) string {
	for i := len(sipHost) - 1; i >= 0; i-- {
		if sipHost[i] == ':' {
			return sipHost[:i]
		}
	}
	return sipHost
}
