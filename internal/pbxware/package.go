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

// swarmDialerPackageLimit is applied to every per-tenant resource category
// a package gates (extensions, voicemails, queues, IVRs, conferences, ring
// groups, hot desking). Raised from 1000 to 2000 alongside the switch to
// 4-digit Multi-Tenant extensions and the dashboard's +1000 dialer button
// (1000 concurrent local calls needs 2000 extensions, 2 per call) — a
// package capping extensions below what the tenant is asked to create
// would silently cap provisioning short of what was requested.
const swarmDialerPackageLimit = 2000

// packageParams builds the pbxware.package.add/edit request body for
// SwarmDialer's fixed load-testing profile: generous limits everywhere,
// every optional feature (call recording, monitoring, call screening,
// restricted service plans) turned off, since none of it matters for load
// testing and only adds noise/risk of hitting a limit unrelated to what's
// actually being tested.
//
// Required field names are a different shape than what
// package.configuration reports back for the same values (pluralized/
// renamed: extensions/voicemails/ivrs/cfs vs. ext/voicemail/ivr/cf) —
// found empirically by iterating on the API's own "Required field 'X' is
// missing" errors, since this isn't documented anywhere we've found.
func packageParams(name string) url.Values {
	limit := strconv.Itoa(swarmDialerPackageLimit)
	return url.Values{
		"name":            {name},
		"extensions":      {limit},
		"voicemails":      {limit},
		"queues":          {limit},
		"ivrs":            {limit},
		"cfs":             {limit}, // conferences
		"rgroups":         {limit}, // ring groups
		"hot_desking":     {limit},
		"restrict_splans": {"0"}, // restrict service plans: no
		"call_recordings": {"0"},
		"monitoring":      {"0"},
		"call_screening":  {"0"},
	}
}

// AddPackage creates a tenant package with SwarmDialer's fixed profile —
// see packageParams.
func (c *Client) AddPackage(name string) (int, error) {
	body, err := c.call("pbxware.package.add", packageParams(name))
	if err != nil {
		return 0, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return 0, fmt.Errorf("package.add succeeded but response had no usable id: %w", err)
	}
	return id, nil
}

// UpdatePackage re-applies SwarmDialer's fixed profile (see packageParams)
// to an existing package — used by EnsureSwarmDialerPackage so a package
// created by an older version of this tool (e.g. with the previous 1000
// limit) gets corrected automatically instead of silently staying stale.
func (c *Client) UpdatePackage(id int, name string) error {
	params := packageParams(name)
	params.Set("id", strconv.Itoa(id))
	_, err := c.call("pbxware.package.edit", params)
	return err
}

// DeletePackage deletes a tenant package — server must be 1, same
// convention as AddPackage/UpdatePackage's implicit system-level scope.
func (c *Client) DeletePackage(id int) error {
	_, err := c.call("pbxware.package.delete", url.Values{
		"server": {"1"},
		"id":     {strconv.Itoa(id)},
	})
	return err
}

// EnsureSwarmDialerPackage returns the ID of the SwarmDialerPackageName
// tenant package, creating it if it doesn't already exist — or updating
// it to the current profile if it does, the same "always re-apply,
// whether new or reused" approach as ResaveTenant. Packages are
// system-level, not per-tenant, so on a PBXware instance SwarmDialer has
// already provisioned once, later tenants reuse (and refresh) the same
// package rather than creating a duplicate every time.
func (c *Client) EnsureSwarmDialerPackage() (int, error) {
	packages, err := c.ListPackages()
	if err != nil {
		return 0, fmt.Errorf("listing packages: %w", err)
	}
	for id, name := range packages {
		if name == SwarmDialerPackageName {
			if err := c.UpdatePackage(id, name); err != nil {
				return 0, fmt.Errorf("updating existing tenant package: %w", err)
			}
			return id, nil
		}
	}
	return c.AddPackage(SwarmDialerPackageName)
}
