package pbxware

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// TrunkProvider is one entry from pbxware.trunk.providers.
type TrunkProvider struct {
	Name string
	ID   int
	Type string // "pstn" or "voip"
}

// ListTrunkProviders returns all trunk providers available on this
// instance. Used to find "Generic SIP"'s provider ID dynamically rather
// than hardcoding it — confirmed stable at 20 on our test instance, but
// SwarmDialer is meant to run against arbitrary fresh PBXware installs
// (see docs/gui_spec.md's portability requirement), so don't assume it.
func (c *Client) ListTrunkProviders(tenantID int) ([]TrunkProvider, error) {
	body, err := c.call("pbxware.trunk.providers", url.Values{"server": {strconv.Itoa(tenantID)}})
	if err != nil {
		return nil, err
	}
	var out []TrunkProvider
	for name, raw := range body {
		pair, ok := raw.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		idStr, _ := pair[0].(string)
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		typ, _ := pair[1].(string)
		out = append(out, TrunkProvider{Name: name, ID: id, Type: typ})
	}
	return out, nil
}

// GenericSIPProviderID finds "Generic SIP"'s provider ID on this instance.
func (c *Client) GenericSIPProviderID(tenantID int) (int, error) {
	providers, err := c.ListTrunkProviders(tenantID)
	if err != nil {
		return 0, err
	}
	for _, p := range providers {
		if p.Name == "Generic SIP" {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("no %q provider found on this instance", "Generic SIP")
}

// TrunkParams holds the fields needed to create a SIP trunk between two
// PBXware instances. Host/Username/Secret describe this instance's side;
// PeerHost/PeerSecret describe the far side. PeerHost also doubles as the
// trunk's "Incoming IP addresses" ACL (from_ipaddr) — see AddTrunk. See
// docs/pbxware_api_reference.md's Trunks section for the full field
// reference.
type TrunkParams struct {
	Server        int // Tenant/Server ID
	Name          string
	ProviderID    int    // Generic SIP's ID — see GenericSIPProviderID
	Host          string // this side's host
	Username      string // this side's auth username
	Secret        string // this side's auth secret
	PeerHost      string // far side's host
	PeerSecret    string // far side's auth secret
	Country       string
	National      string
	International string
}

// TrunkResult is what AddTrunk returns on success.
type TrunkResult struct {
	TrunkID int
}

// AddTrunk creates a SIP trunk. Codec/DTMF/etc. are fixed to sensible
// defaults for load testing (SwarmDialerCodecsList, RFC2833 DTMF) —
// SwarmDialer doesn't need this to be configurable beyond what TrunkParams
// exposes. The trunk must allow every codec the dialer can place calls
// with: with only ulaw/alaw allowed (the original setting), remote G.729
// calls failed outright with 503 (PBXware can't transcode G.729) and
// remote G.722/Opus calls were silently transcoded to ulaw on the trunk —
// confirmed live 2026-09-26.
//
// type=peer + passthru_mode=yes + peer_username="admin" (a fixed literal,
// not a per-deployment generated value) + from_ipaddr=PeerHost +
// insecure=very + register=0 (Not Required), replacing an earlier
// type=friend trunk with generated/cross-referenced peer credentials and
// insecure=port,invite: that setup let calls signal as answered without
// actually reaching the far instance (PBXware playing its own local
// "destination_not_recognized" announcement, which Asterisk answers
// before playing — a false positive from SwarmDialer's signaling-only
// point of view). incominglimit/outgoinglimit raised 600 -> 1000 as part
// of the same fix. Confirmed working live 2026-09-20 with real
// bidirectional RTP between the two instances (not just local signaling).
func (c *Client) AddTrunk(p TrunkParams) (TrunkResult, error) {
	params := url.Values{
		"server":        {strconv.Itoa(p.Server)},
		"name":          {p.Name},
		"provider_id":   {strconv.Itoa(p.ProviderID)},
		"type":          {"peer"},
		"passthru_mode": {"yes"},
		"dtmfmode":      {"rfc2833"},
		"status":        {"active"},
		"country":       {p.Country},
		"national":      {p.National},
		"international": {p.International},
		"emerg_trunk":   {"no"},
		"host":          {p.Host},
		"username":      {p.Username},
		"secret":        {p.Secret},
		"peer_host":     {p.PeerHost},
		"peer_username": {"admin"},
		"peer_secret":   {p.PeerSecret},
		"from_ipaddr":   {p.PeerHost},
		"insecure":      {"very"},
		"register":      {"0"},
		"looserouting":  {"yes"},
		"incominglimit": {"1000"},
		"outgoinglimit": {"1000"},
		"codecs":        {strings.Join(SwarmDialerCodecsList, ",")},
		"codecs_ptime":  {trunkCodecPtimes()}, // one ptime per codec — must match codecs' item count
	}
	body, err := c.call("pbxware.trunk.add", params)
	if err != nil {
		return TrunkResult{}, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return TrunkResult{}, fmt.Errorf("trunk.add succeeded but response had no usable id: %w", err)
	}
	return TrunkResult{TrunkID: id}, nil
}

// trunkCodecPtimes returns "20,20,..." with one 20ms ptime per entry in
// SwarmDialerCodecsList, as trunk.add requires.
func trunkCodecPtimes() string {
	ptimes := make([]string, len(SwarmDialerCodecsList))
	for i := range ptimes {
		ptimes[i] = "20"
	}
	return strings.Join(ptimes, ",")
}

// SetTenantDefaultTrunk sets trunkID as tenantID's primary (and only, for
// SwarmDialer's purposes) outbound trunk — Multi-Tenant only, there's no
// equivalent for non-Multi-Tenant/system-level editions (confirmed live:
// pbxware.tenant.trunks.list/set on a non-tenant-mode instance returns
// "Tenant mode is not enabled").
//
// This step is not optional, despite an earlier (same-physical-instance)
// test suggesting it was: creating the trunk and DIDs alone is enough for
// *inbound* routing (dialing a DID from the outside reaches the mapped
// extension fine), but a tenant's own extensions placing *outbound* calls
// over a trunk that was never set as the tenant's default trunk signal as
// answered (a real 200 OK) while never actually establishing media —
// confirmed live (2026-09-19): RTP was one-way or absent entirely (0
// packets received back) until this was set, then real bidirectional RTP
// flowed immediately. The earlier finding only held because that test
// used two tenants on the *same* physical PBXware instance, where local-
// channel routing apparently doesn't need a designated default trunk the
// way a real cross-instance trunk hop does.
//
// The action name doesn't follow the usual two-part object.method
// convention — it's the three-part pbxware.tenant.trunks.list/set, found
// by noticing a PHP "Undefined array key 3" warning on the two-part
// pbxware.tenant.trunks, which was the API's own hint that a fourth
// dot-separated segment was expected.
func (c *Client) SetTenantDefaultTrunk(tenantID, trunkID int) error {
	_, err := c.call("pbxware.tenant.trunks.set", url.Values{
		"tenant":        {strconv.Itoa(tenantID)},
		"trunks":        {strconv.Itoa(trunkID)},
		"primary_trunk": {strconv.Itoa(trunkID)},
	})
	return err
}

// DeleteTrunk deletes a trunk — trunks are system-level objects (server=1)
// even in Tenant Mode, same as AddTrunk.
func (c *Client) DeleteTrunk(server, trunkID int) error {
	_, err := c.call("pbxware.trunk.delete", url.Values{
		"server": {strconv.Itoa(server)},
		"id":     {strconv.Itoa(trunkID)},
	})
	return err
}
