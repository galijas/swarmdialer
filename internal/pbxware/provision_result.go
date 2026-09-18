package pbxware

// ProvisionedExtension is one extension created by the provisioning CLI
// (cmd/swarmdialer), with everything needed to register it as a SIP
// endpoint later.
type ProvisionedExtension struct {
	ExtensionID int    `json:"extension_id"`
	Ext         string `json:"ext"`
	Username    string `json:"username"`
	Secret      string `json:"secret"`
}

// ProvisionResult is the JSON shape written by cmd/swarmdialer's
// provisioning step and read by cmd/loadtest — the hand-off between
// provisioning and call generation.
type ProvisionResult struct {
	Server     int                     `json:"server"`
	Host       string                  `json:"host"`
	Extensions []ProvisionedExtension  `json:"extensions"`
}
