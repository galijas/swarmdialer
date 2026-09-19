package pbxware

import (
	"fmt"
	"net/url"
	"strconv"
)

// DIDParams holds the fields needed to route one DID number to one
// extension over a trunk — see docs/pbxware_api_reference.md's DIDs
// section. Destination is the plain extension number (confirmed live via
// pbxware.did.list showing the correct "ext" after creation — not the
// extension's internal ID).
type DIDParams struct {
	Server      int    // Tenant/Server ID
	TrunkID     int    // Trunk this DID is reached through
	DID         string // the actual DID number
	Destination string // target extension (number or ID — see doc comment)
}

// DIDResult is what AddDID returns on success.
type DIDResult struct {
	DIDID int
}

// AddDID creates one DID routed to an extension (dest_type=0).
func (c *Client) AddDID(p DIDParams) (DIDResult, error) {
	body, err := c.call("pbxware.did.add", url.Values{
		"server":      {strconv.Itoa(p.Server)},
		"trunk":       {strconv.Itoa(p.TrunkID)},
		"did":         {p.DID},
		"dest_type":   {"0"},
		"destination": {p.Destination},
		"disabled":    {"0"},
	})
	if err != nil {
		return DIDResult{}, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return DIDResult{}, fmt.Errorf("did.add succeeded but response had no usable id: %w", err)
	}
	return DIDResult{DIDID: id}, nil
}
