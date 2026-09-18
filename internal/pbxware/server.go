package pbxware

// GetSystemExtensionLength returns the number of digits PBXware expects
// for extension numbers on a non-Multi-Tenant (system-level) instance —
// the "numbering" field from pbxware.server.configuration. Multi-Tenant
// editions choose this freely per tenant (see TenantParams.ExtLength);
// non-Multi-Tenant editions have a single system-wide value we must
// detect and match, not choose ourselves — confirmed live: creating a
// 3-digit extension ("100") against a system configured for 4 was
// rejected with "Required field 'ext=100' contains invalid data
// (Regex: /^\d{4}$/)", every single time, for the full provisioning
// retry window, since AddExtensionWithRetry can't distinguish that from
// a transient error.
func (c *Client) GetSystemExtensionLength() (int, error) {
	body, err := c.call("pbxware.server.configuration", nil)
	if err != nil {
		return 0, err
	}
	return toInt(body["numbering"])
}
