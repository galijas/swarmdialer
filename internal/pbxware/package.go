package pbxware

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SwarmDialerPackageName is the tenant package SwarmDialer creates (or
// reuses) on every Multi-Tenant PBXware instance it provisions against.
// Multi-Tenant editions require an existing package to reference when
// creating a tenant — see EnsureSwarmDialerPackage.
const SwarmDialerPackageName = "SwarmDialerPackage"

// ListPackages returns every tenant package on the system, ID -> name, as
// returned by pbxware.package.list. A PBXware instance with zero packages
// (e.g. a genuinely fresh Multi-Tenant install) returns an *error*
// response ("No tenant packages present on system.") instead of an empty
// list for this action — confirmed live against a fresh instance — so
// that specific error is treated as zero packages, not a failure.
func (c *Client) ListPackages() (map[int]string, error) {
	body, err := c.call("pbxware.package.list", nil)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no tenant packages") {
			return map[int]string{}, nil
		}
		return nil, err
	}
	out := make(map[int]string, len(body))
	for idStr, nameRaw := range body {
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		out[id] = fmt.Sprint(nameRaw)
	}
	return out, nil
}

// AddPackage creates a tenant package with SwarmDialer's fixed load-testing
// profile: generous (1000) limits on every per-tenant resource category a
// package gates, and every optional feature (call recording, monitoring,
// call screening, restricted service plans) turned off, since none of it
// matters for load testing and only adds noise/risk of hitting a limit
// unrelated to what's actually being tested.
//
// package.add's required field names are a different shape than what
// package.configuration reports back for the same values (pluralized/
// renamed: extensions/voicemails/ivrs/cfs vs. ext/voicemail/ivr/cf) —
// found empirically by iterating on the API's own "Required field 'X' is
// missing" errors, since this isn't documented anywhere we've found.
func (c *Client) AddPackage(name string) (int, error) {
	body, err := c.call("pbxware.package.add", url.Values{
		"name":            {name},
		"extensions":      {"1000"},
		"voicemails":      {"1000"},
		"queues":          {"1000"},
		"ivrs":            {"1000"},
		"cfs":             {"1000"}, // conferences
		"rgroups":         {"1000"}, // ring groups
		"hot_desking":     {"1000"},
		"restrict_splans": {"0"}, // restrict service plans: no
		"call_recordings": {"0"},
		"monitoring":      {"0"},
		"call_screening":  {"0"},
	})
	if err != nil {
		return 0, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return 0, fmt.Errorf("package.add succeeded but response had no usable id: %w", err)
	}
	return id, nil
}

// EnsureSwarmDialerPackage returns the ID of the SwarmDialerPackageName
// tenant package, creating it if it doesn't already exist. Packages are
// system-level, not per-tenant, so on a PBXware instance SwarmDialer has
// already provisioned once, later tenants reuse the same package rather
// than creating a duplicate every time.
func (c *Client) EnsureSwarmDialerPackage() (int, error) {
	packages, err := c.ListPackages()
	if err != nil {
		return 0, fmt.Errorf("listing packages: %w", err)
	}
	for id, name := range packages {
		if name == SwarmDialerPackageName {
			return id, nil
		}
	}
	return c.AddPackage(SwarmDialerPackageName)
}
