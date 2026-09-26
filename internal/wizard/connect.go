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
	userA, userB := trunkUsername(a.ID), trunkUsername(b.ID)

	// Peer trunk credentials use a fixed peer_username ("admin") rather
	// than a cross-referenced generated one — see AddTrunk.
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
		PeerHost: bHost, PeerSecret: credB,
		Country: "869", National: "1", International: "011",
	})
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return fmt.Errorf("creating trunk on %s: %w", a.Name, err)
	}

	trunkB, err := clientB.AddTrunk(pbxware.TrunkParams{
		Server: 1, Name: "SwarmDialer-to-" + a.Name, ProviderID: providerIDB,
		Host: bHost, Username: userB, Secret: credB,
		PeerHost: aHost, PeerSecret: credA,
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

	// A new trunk isn't usable for routing straight away — dialing a
	// fresh DID through a fresh trunk failed with "No such extension/
	// context ... while calling Local channel", then worked ~90s later with
	// no other change. The cause (found 2026-09-26): PBXware writes PJSIP
	// config immediately but Asterisk only reloads it on PBXware's own
	// 1-minute cycle, and no API call can trigger or confirm that reload.
	// 90s covers one full cycle with margin.
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
		did := didFor10DigitRoute(srv.TenantCode, ext.Ext)
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

// didFor10DigitRoute builds a DID matching PBXware's built-in default
// "10-digit dialing" route (Start digits: Any, Required Length: 10,
// Regex: [2-9][0-8][0-9][2-9]) — a dialed number has to satisfy some
// configured Route before PBXware will send it out over a trunk at all,
// and creating a custom Route isn't possible through the public API (it's
// a session-authenticated admin-panel-only feature — see PROJECT_STATE.md
// for how this was confirmed), so DIDs are shaped to fit an existing
// default route instead of needing a new one.
//
// Positions 1, 2, and 4 are pinned to '5' (valid under all three of the
// route's character classes: [2-9], [0-8], [2-9]); the other 7 digits
// (position 3, then 5-10) are free and carry tenantCode+ext — 3+4 digits,
// matching the wizard's fixed tenant-code range (200-999) and ext_length
// (4) exactly. tenantCode is "" for non-Multi-Tenant editions, treated as
// "000" so the encoding is uniform either way.
func didFor10DigitRoute(tenantCode, ext string) string {
	v := tenantCode + ext
	for len(v) < 7 {
		v = "0" + v
	}
	return "55" + v[:1] + "5" + v[1:]
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

// trunkUsername derives a trunk auth username from a server ID. PBXware 8.2
// rejects trunk usernames over 20 characters, and "swarm"+ID ("srv-" plus a
// 19-digit nanosecond timestamp) is 28, so only the ID's last 10 digits are
// kept — still distinct between the two servers of a pair, which are
// created seconds apart.
func trunkUsername(serverID string) string {
	id := strings.TrimPrefix(serverID, "srv-")
	if len(id) > 10 {
		id = id[len(id)-10:]
	}
	return "swarm" + id
}
