// Package pbxware is a client for the PBXware HTTP admin API, scoped to what
// SwarmDialer needs: creating tenants and extensions for load testing.
package pbxware

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a single PBXware instance's HTTP API.
type Client struct {
	BaseURL string // e.g. "https://10.1.100.208"
	APIKey  string
	http    *http.Client
}

// NewClient builds a Client. PBXware's test certs are self-signed, so TLS
// verification is skipped — fine for internal load-testing use, not for
// anything public-facing.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

// apiError is returned whenever PBXware's response body contains an "error" key.
type apiError struct {
	Message string
}

func (e *apiError) Error() string { return e.Message }

// call performs one action=pbxware.*.* request and returns the decoded JSON body.
func (c *Client) call(action string, params url.Values) (map[string]any, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("apikey", c.APIKey)
	params.Set("action", action)

	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", action, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response for %s: %w", action, err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		// PBXware's PHP backend serializes an empty associative array as a
		// JSON array ("[]") rather than an object ("{}") — confirmed live
		// on pbxware.tenant.list once the last tenant is deleted (PHP
		// can't tell an empty map from an empty list, so json_encode
		// defaults to the array form). Treat an empty array the same as
		// an empty object; anything else is a genuine decode failure.
		var arr []any
		if err2 := json.Unmarshal(raw, &arr); err2 == nil && len(arr) == 0 {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("decoding response for %s: %w", action, err)
	}

	if msg, ok := body["error"].(string); ok {
		return body, &apiError{Message: msg}
	}
	return body, nil
}

// TenantParams holds the fields needed to create a tenant.
// See docs/pbxware_api_reference.md for the full field reference.
type TenantParams struct {
	Name          string // FQDN-style name, e.g. "swarmdialer.local"
	Code          string // 3-digit code; on our test instance the valid range is 200-999
	Package       string // Tenant package ID, e.g. "1"
	ExtLength     int    // extension number length, cannot be changed later
	Country       string // route ID, e.g. "869" for USA
	National      string // national dialing code
	International string // international dialing code
}

// AddTenant creates a tenant and returns its Tenant/Server ID.
//
// PBXware's tenant.add call frequently outlasts a normal HTTP client timeout
// even when it ultimately succeeds (tenant DB provisioning is slow) — a
// timeout/transport error here does NOT necessarily mean the tenant wasn't
// created. Callers should fall back to ListTenants to check before retrying,
// to avoid creating duplicate tenants with a different code.
func (c *Client) AddTenant(p TenantParams) (int, error) {
	params := url.Values{
		"tenant_name":   {p.Name},
		"tenant_code":   {p.Code},
		"package":       {p.Package},
		"ext_length":    {strconv.Itoa(p.ExtLength)},
		"country":       {p.Country},
		"national":      {p.National},
		"international": {p.International},
	}
	body, err := c.call("pbxware.tenant.add", params)
	if err != nil {
		return 0, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return 0, fmt.Errorf("tenant.add succeeded but response had no usable id: %w", err)
	}
	return id, nil
}

// DeleteTenant deletes a tenant — server must be 1 per the API's own
// convention for tenant-level actions (see AddTenant), even though the
// tenant being deleted has its own separate tenant/server ID.
func (c *Client) DeleteTenant(tenantID int) error {
	_, err := c.call("pbxware.tenant.delete", url.Values{
		"server": {"1"},
		"id":     {strconv.Itoa(tenantID)},
	})
	return err
}

// TenantChannelLimits is a tenant's current concurrent-resource capacity,
// as read back from pbxware.tenant.configuration. PBXware tracks these as
// separate, independently-defaulted pools — Local/Remote SIP channels are
// only one of them. Confirmed empirically (2026-09-18): a tenant with
// Local/Remote channels correctly raised to 600 still hit a hard wall at 8
// concurrent local extension-to-extension calls, because Conferences,
// Queues, Enhanced Ring Groups, and DAHDI *also* independently default to
// 8 and PBXware's local-dialing AGI is gated by (or otherwise touches) one
// or more of them even for a plain two-extension call with none of those
// features involved. Auto Attendants defaults to a low value too and was
// raised for the same reason, even though its cause wasn't isolated as
// precisely. All five must be raised together, not just Local/Remote.
type TenantChannelLimits struct {
	Local              int
	Remote             int
	Conferences        int
	Queues             int
	EnhancedRingGroups int
	AutoAttendants     int
	DAHDI              int
}

// GetTenantChannelLimits reads a tenant's current channel-like limits — see
// ResaveTenant for the request/response field-name mismatches
// (only Auto Attendants' request and response names happen to match).
func (c *Client) GetTenantChannelLimits(tenantID int) (TenantChannelLimits, error) {
	body, err := c.call("pbxware.tenant.configuration", url.Values{"id": {strconv.Itoa(tenantID)}})
	if err != nil {
		return TenantChannelLimits{}, err
	}
	fields := map[string]*int{}
	limits := TenantChannelLimits{}
	fields["incominglimit"] = &limits.Local
	fields["outgoinglimit"] = &limits.Remote
	fields["conch"] = &limits.Conferences
	fields["quech"] = &limits.Queues
	fields["ergch"] = &limits.EnhancedRingGroups
	fields["aach"] = &limits.AutoAttendants
	fields["zapch"] = &limits.DAHDI
	for respField, dst := range fields {
		v, err := toInt(body[respField])
		if err != nil {
			return TenantChannelLimits{}, fmt.Errorf("reading %s: %w", respField, err)
		}
		*dst = v
	}
	return limits, nil
}

// ResaveTenant raises (or sets) every one of a tenant's channel-like
// concurrency pools to the same limit — Local/Remote SIP channels,
// Conferences, Queues, Enhanced Ring Groups, Auto Attendants, and DAHDI
// (see TenantChannelLimits) — and, in the same tenant.edit call, resends
// the tenant's own current name/code/package/ext_length/country/national/
// international unchanged.
//
// That second part isn't optional. Confirmed live (2026-09-19): a
// tenant.edit that sends *only* the channel-limit fields can leave a
// freshly-created tenant unable to actually place or receive calls —
// every SIP REGISTER rejected outright with 403, or every INVITE declined
// with 603 — even though every limit reads back correctly afterward. This
// is the exact effect a manual "Save" on the tenant in PBXware's admin
// GUI fixes, and the GUI's Save resubmits the tenant's whole form, not
// just the fields being changed. A previous version of this function only
// ever sent the channel-limit fields and could leave a tenant in this
// broken state with no error raised anywhere — the limits looked correct,
// calls just silently failed. Fetching the tenant's current identity
// fields and resending them alongside the channel limits in one edit
// reproduces the GUI's fix; the narrow edit alone does not.
//
// Timing matters too, and isn't fully solved by this function alone: a
// resave done immediately after tenant creation reliably fixes SIP
// REGISTER-level rejection (403/500) but can still leave calls between
// extensions declining with 603 shortly afterward — the same eventually-
// consistent-backend pattern already seen elsewhere in this API (e.g. the
// ~90s trunk settling wait in wizard.ConnectServers): the DB write and
// this function's own read-back both succeed fast, but whatever PBXware
// does at the Asterisk/dialplan level to actually act on it lags behind.
// A second ResaveTenant call once real wall-clock time has passed (e.g.
// after extension creation finishes, not immediately after tenant
// creation) has fixed this live — see wizard.Provision, which calls this
// twice for exactly that reason.
//
// PBXware defaults every one of the channel-like pools to 8 independently,
// regardless of package or license (no per-tenant license limit exists;
// only a system-wide total channel count is licensed), and none of them
// are settable by sending back the response field name as a request param
// (e.g. incominglimit/outgoinglimit, conch/quech/ergch/zapch as request
// params are silently ignored — only aach happens to double as its own
// request param name). The request must use these differently-named
// parameters instead:
//
//	response field -> request param
//	incominglimit   -> local_channels
//	outgoinglimit   -> remote_channels
//	conch           -> conferences
//	quech           -> queues
//	ergch           -> enhanced_ring_groups
//	aach            -> aach
//	zapch           -> dahdi
//
// This is exactly the settings under PBXware's admin GUI at System level →
// Tenants → <tenant> → Advanced settings → Channels.
//
// SwarmDialer needs all of these raised well above 8 for any load test
// beyond trivial scale — call this as a standard step after WaitForTenant
// whenever setting up or reusing a tenant for load testing, not just when
// creating a brand-new one, since existing/GUI-created tenants default to
// 8 too.
//
// The underlying tenant.edit call follows the same slow-write pattern as
// AddTenant (frequently outlasts the HTTP client's own timeout even when it
// succeeds server-side), so this retries with backoff and verifies via
// GetTenantChannelLimits rather than trusting the HTTP response alone —
// note that verification only covers the channel limits actually applying,
// since there's no API-visible signal for "the registration/dialplan
// brokenness this also fixes is gone."
// SwarmDialerCodecs is the fixed set of codecs SwarmDialer enables
// wherever it provisions (tenant-level allowlist via ResaveTenant, and
// per-extension via ExtensionParams.AllowedCodecs) — everything the
// dashboard's codec dropdown offers (see sipua.Codec) except GSM, which
// has no usable pure-Go encoder (see sipua/codec.go). Colon-separated,
// matching both fields' documented request format.
const SwarmDialerCodecs = "ulaw:alaw:g722:g729:opus"

// SwarmDialerCodecsList is SwarmDialerCodecs as a slice — v2's codecs
// fields (ClientV2.PatchTenant) take real JSON arrays instead of a
// colon-separated string.
var SwarmDialerCodecsList = []string{"ulaw", "alaw", "g722", "g729", "opus"}

func (c *Client) ResaveTenant(tenantID, limit, extLength int, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 5 * time.Second
	const maxDelay = 30 * time.Second

	matches := func(l TenantChannelLimits) bool {
		return l.Local == limit && l.Remote == limit && l.Conferences == limit &&
			l.Queues == limit && l.EnhancedRingGroups == limit && l.AutoAttendants == limit && l.DAHDI == limit
	}

	config, err := c.call("pbxware.tenant.configuration", url.Values{"id": {strconv.Itoa(tenantID)}})
	if err != nil {
		return fmt.Errorf("reading tenant %d's configuration before resaving: %w", tenantID, err)
	}
	identityFields := url.Values{
		"tenant_name":   {fmt.Sprint(config["server_name"])},
		"tenant_code":   {fmt.Sprint(config["tenantcode"])},
		"package":       {fmt.Sprint(config["package_id"])},
		"ext_length":    {strconv.Itoa(extLength)},
		"country":       {fmt.Sprint(config["country"])},
		"national":      {fmt.Sprint(config["national"])},
		"international": {fmt.Sprint(config["international"])},
	}

	for {
		params := url.Values{
			"server":               {"1"},
			"id":                   {strconv.Itoa(tenantID)},
			"local_channels":       {strconv.Itoa(limit)},
			"remote_channels":      {strconv.Itoa(limit)},
			"conferences":          {strconv.Itoa(limit)},
			"queues":               {strconv.Itoa(limit)},
			"enhanced_ring_groups": {strconv.Itoa(limit)},
			"aach":                 {strconv.Itoa(limit)},
			"dahdi":                {strconv.Itoa(limit)},
			// Per-extension acodecs (see ExtensionParams.AllowedCodecs)
			// can only select from what's allowed at the tenant level —
			// confirmed live 2026-09-23: setting an extension's acodecs
			// to a codec the tenant doesn't itself allow gets silently
			// dropped, not rejected, which is easy to miss.
			"localcodecs":  {SwarmDialerCodecs},
			"remotecodecs": {SwarmDialerCodecs},
		}
		for k, v := range identityFields {
			params[k] = v
		}
		_, editErr := c.call("pbxware.tenant.edit", params)

		current, readErr := c.GetTenantChannelLimits(tenantID)
		if readErr == nil && matches(current) {
			return nil
		}

		if time.Now().Add(delay).After(deadline) {
			if editErr != nil {
				return fmt.Errorf("gave up after %s waiting for channel limits to apply: %w", maxWait, editErr)
			}
			return fmt.Errorf("gave up after %s: tenant %d channel limits still not fully applied (currently %+v)", maxWait, tenantID, current)
		}
		time.Sleep(delay)
		delay = min(delay*2, maxDelay)
	}
}

// Tenant is one entry from ListTenants.
type Tenant struct {
	ID   int
	Name string
	Code string
}

// ListTenants returns all tenants currently on the system.
func (c *Client) ListTenants() ([]Tenant, error) {
	body, err := c.call("pbxware.tenant.list", nil)
	if err != nil {
		return nil, err
	}
	var out []Tenant
	for idStr, raw := range body {
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Tenant{
			ID:   id,
			Name: fmt.Sprint(obj["name"]),
			Code: fmt.Sprint(obj["tenantcode"]),
		})
	}
	return out, nil
}

// ExtensionParams holds the fields needed to create an extension.
type ExtensionParams struct {
	Server        int    // Tenant/Server ID
	Name          string // Full name
	Email         string
	Ext           string // extension number; leave empty to let PBXware assign one, if supported
	Secret        string // SIP password; must satisfy PBXware's complexity rule (see GenerateSecret)
	UA            int    // User Agent Device ID; 50 = Generic SIP on the instances we've tested
	IncomingLimit int
	OutgoingLimit int
	AllowedCodecs string // colon-separated, e.g. "ulaw:alaw"
}

// ExtensionResult is what AddExtension returns on success.
type ExtensionResult struct {
	ExtensionID int
	Ext         int
}

// AddExtension creates one extension under an existing tenant.
//
// This is the call most likely to fail with a transient "tenant isn't fully
// provisioned yet" error (see AddExtensionWithRetry) — PBXware needs several
// minutes to finish setting up a freshly created tenant's database before
// extensions can be added to it.
func (c *Client) AddExtension(p ExtensionParams) (ExtensionResult, error) {
	params := url.Values{
		"server":        {strconv.Itoa(p.Server)},
		"name":          {p.Name},
		"email":         {p.Email},
		"location":      {"1"}, // Local
		"ua":            {strconv.Itoa(p.UA)},
		"status":        {"1"}, // Active
		"pin":           {"1234"},
		"incominglimit": {strconv.Itoa(p.IncomingLimit)},
		"outgoinglimit": {strconv.Itoa(p.OutgoingLimit)},
		"voicemail":     {"0"},
		"prot":          {"sip"},
		"secret":        {p.Secret},
	}
	if p.Ext != "" {
		params.Set("ext", p.Ext)
	}
	if p.AllowedCodecs != "" {
		params.Set("acodecs", p.AllowedCodecs)
	}

	body, err := c.call("pbxware.ext.add", params)
	if err != nil {
		return ExtensionResult{}, err
	}
	id, err := toInt(body["id"])
	if err != nil {
		return ExtensionResult{}, fmt.Errorf("ext.add succeeded but response had no usable id: %w", err)
	}
	ext, _ := toInt(body["ext"]) // best-effort; not fatal if PBXware omits it
	return ExtensionResult{ExtensionID: id, Ext: ext}, nil
}

// DeleteExtension deletes one extension.
func (c *Client) DeleteExtension(server, extensionID int) error {
	_, err := c.call("pbxware.ext.delete", url.Values{
		"server": {strconv.Itoa(server)},
		"id":     {strconv.Itoa(extensionID)},
	})
	return err
}

// ExtensionConfig is the subset of pbxware.ext.configuration we need to build
// SIP registration credentials for a created extension.
type ExtensionConfig struct {
	Ext      string
	Secret   string
	Username string // what to register with; tenant_code + ext, per PBXware convention

	// AllowedCodecsRaw is the extension's "allow" list as PBXware reports
	// it back (e.g. "ulaw,0 alaw,0 g722,0"), not just what was requested —
	// a per-extension acodecs value can only include what the system (or,
	// for Multi-Tenant, the tenant) already allows.
	AllowedCodecsRaw string
}

// GetExtensionConfig fetches an extension's SIP registration details.
func (c *Client) GetExtensionConfig(server, extensionID int) (ExtensionConfig, error) {
	body, err := c.call("pbxware.ext.configuration", url.Values{
		"server": {strconv.Itoa(server)},
		"id":     {strconv.Itoa(extensionID)},
	})
	if err != nil {
		return ExtensionConfig{}, err
	}
	entry, ok := body[strconv.Itoa(extensionID)].(map[string]any)
	if !ok {
		return ExtensionConfig{}, fmt.Errorf("unexpected ext.configuration response shape")
	}
	options, _ := entry["options"].(map[string]any)
	allow, _ := options["allow"].([]any)
	allowStrs := make([]string, 0, len(allow))
	for _, a := range allow {
		allowStrs = append(allowStrs, fmt.Sprint(a))
	}
	return ExtensionConfig{
		Ext:              fmt.Sprint(entry["ext"]),
		Secret:           fmt.Sprint(options["secret"]),
		Username:         fmt.Sprint(options["username"]),
		AllowedCodecsRaw: strings.Join(allowStrs, " "),
	}, nil
}

func toInt(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case string:
		return strconv.Atoi(t)
	default:
		return 0, fmt.Errorf("unexpected type %T for numeric field", v)
	}
}
