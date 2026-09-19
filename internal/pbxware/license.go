package pbxware

import "strings"

// LicenseInfo is the subset of pbxware.license.info's response SwarmDialer
// cares about: which edition this instance is running (to decide whether
// tenant creation applies at all) and the real license limits, so wizard
// inputs can be validated against them instead of just showing a generic
// warning.
type LicenseInfo struct {
	Edition    string
	Channels   int
	Extensions int
	Tenants    int
	DIDs       int
	VOIPTrunks int
	PSTNTrunks int
}

// IsMultiTenant reports whether this instance's edition supports tenant
// creation. Only "Multi-Tenant" is confirmed from real testing — any other
// edition string is treated as "skip tenant creation, provision extensions
// at the system level directly" (server ID 1), since the exact strings for
// other editions (e.g. Business, Contact Centre) aren't documented
// anywhere we've found. See docs/pbxware_api_reference.md's License
// section for that gap.
func (l LicenseInfo) IsMultiTenant() bool {
	return strings.EqualFold(l.Edition, "Multi-Tenant")
}

// GetLicenseInfo fetches the current license's edition and limits.
func (c *Client) GetLicenseInfo() (LicenseInfo, error) {
	body, err := c.call("pbxware.license.info", nil)
	if err != nil {
		return LicenseInfo{}, err
	}
	return LicenseInfo{
		Edition:    stringField(body, "Edition"),
		Channels:   intField(body, "Channels"),
		Extensions: intField(body, "Extensions"),
		Tenants:    intField(body, "Tenants"),
		DIDs:       intField(body, "DIDs"),
		VOIPTrunks: intField(body, "VOIP Trunks"),
		PSTNTrunks: intField(body, "PSTN Trunks"),
	}, nil
}

func stringField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

func intField(body map[string]any, key string) int {
	n, _ := toInt(body[key])
	return n
}
