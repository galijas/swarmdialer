package pbxware

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClientV2 talks to a single PBXware instance's newer v2 (REST/JSON) admin
// API, introduced in 8.2. Bearer-token auth (confirmed live 2026-09-24: the
// API key itself is the bearer token, no separate login/exchange step) and
// true partial PATCH updates — only fields present in the request body
// change, everything else is left alone (confirmed live: PATCHing only
// channels_limit leaves call_recordings, codecs, etc. completely
// untouched). That's a real fix for the exact danger v1's Client.ResaveTenant
// works around by resending the whole tenant form.
//
// v1 and v2 coexist and both stay supported long-term (confirmed with
// Bicom 2026-09-24). This client covers tenant packages, tenants,
// extensions and the system/tenant-level channel, codec and recording
// settings. Trunks stay on v1's Client (by choice — v1's trunk setup is
// already proven for cross-instance calls), and so do DIDs (v2 has no DID
// endpoints yet).
type ClientV2 struct {
	BaseURL string // e.g. "https://10.1.100.208"
	APIKey  string // sent as "Authorization: Bearer <APIKey>"
	http    *http.Client
}

// NewClientV2 builds a ClientV2. Same self-signed-cert allowance as
// NewClient — fine for internal load-testing use only.
func NewClientV2(baseURL, apiKey string) *ClientV2 {
	return &ClientV2{
		BaseURL: strings.TrimRight(baseURL, "/"), // "https://host/" + "/api/..." would 404
		APIKey:  apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

// v2APIError is v2's error response shape — {"code": N, "description":
// "...", "details": [...], "trace_id": "..."} — confirmed live for both a
// validation failure (400) and a bad token (401), consistently, unlike
// v1's ad hoc {"error": "..."} sometimes mixed into an otherwise-
// successful-shaped body. Details carries the actual per-field validation
// messages — Description alone is often just a generic "Bad request."
type v2APIError struct {
	Code        int      `json:"code"`
	Description string   `json:"description"`
	Details     []string `json:"details"`
	TraceID     string   `json:"trace_id"`
}

func (e *v2APIError) Error() string {
	if len(e.Details) > 0 {
		return fmt.Sprintf("pbxware v2 api error %d: %s (%s)", e.Code, e.Description, strings.Join(e.Details, "; "))
	}
	return fmt.Sprintf("pbxware v2 api error %d: %s", e.Code, e.Description)
}

// call performs one v2 REST request. body (if non-nil) is JSON-encoded as
// the request body; out (if non-nil) receives the JSON-decoded response.
func (c *ClientV2) call(method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response for %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= 300 {
		var apiErr v2APIError
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Description != "" {
			return &apiErr
		}
		return fmt.Errorf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, string(raw))
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding response for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// SystemTenantID is the id that always represents the system-wide config
// on v2's /tenants/{id} endpoint — id 1 on every edition. On Multi-Tenant
// it's the "System" record, distinct from any real tenant's own id; on a
// non-Multi-Tenant edition (no separate tenants exist) it IS the whole
// system's config — the same record v1 code already treats as tenantID 1
// for extension operations (see wizard.Provision's isMultiTenant branch).
// Confirmed live 2026-09-24: id 1's response includes stereo-recording and
// RAM-disk fields that never appear on a real Multi-Tenant tenant's own
// record (e.g. id 21) — those two settings are only ever system-level,
// regardless of edition.
const SystemTenantID = 1

// ChannelsLimitV2 is the channel-count subset of a v2 tenant/system config
// record this project reads.
type ChannelsLimitV2 struct {
	Local  int `json:"local"`
	Remote int `json:"remote"`
}

// CodecsV2 is the codec-allowlist subset of a v2 tenant/system config
// record this project reads — same three categories as v1's
// localcodecs/remotecodecs/networkcodecs (see Client.ResaveTenant), but as
// real JSON arrays instead of colon-separated strings.
type CodecsV2 struct {
	Local   []string `json:"local"`
	Remote  []string `json:"remote"`
	Network []string `json:"network"`
}

// CallRecordingsV2 is the call-recording subset of a v2 tenant/system
// config record this project reads. UseRAMDisk/RAMDiskSize and the two
// StereoRecording* fields only ever appear on the system-level record (id
// SystemTenantID) — a real Multi-Tenant tenant's own record omits them
// entirely (see SystemTenantID's doc comment).
type CallRecordingsV2 struct {
	Enabled                string `json:"enabled"`
	Format                 string `json:"format"`
	UseRAMDisk             string `json:"use_ram_disk"`
	RAMDiskSize            int    `json:"ram_disk_size"`
	StereoRecordingEnabled string `json:"stereo_recording_enabled"`
	StereoRecordingFormat  string `json:"stereo_recording_format"`
}

// NumberingDefaultsV2 is the numbering subset of a v2 tenant/system config
// record — on the system record (SystemTenantID) of a non-Multi-Tenant
// edition, ExtLength is the system-wide extension digit length every new
// extension must match.
type NumberingDefaultsV2 struct {
	ExtLength int `json:"ext_length"`
}

// TenantConfigV2 is the subset of a v2 tenant/system config record this
// project reads — PBXware's actual response has many more fields, which
// encoding/json silently ignores on decode.
type TenantConfigV2 struct {
	ID                int                 `json:"id"`
	Name              string              `json:"name"`
	Code              int                 `json:"code"`
	NumberingDefaults NumberingDefaultsV2 `json:"numbering_defaults"`
	ChannelsLimit     ChannelsLimitV2     `json:"channels_limit"`
	Codecs            CodecsV2            `json:"codecs"`
	CallRecordings    CallRecordingsV2    `json:"call_recordings"`
}

// GetTenant reads a tenant/system config record (see SystemTenantID for
// which id means "system-wide").
func (c *ClientV2) GetTenant(id int) (TenantConfigV2, error) {
	var out TenantConfigV2
	err := c.call(http.MethodGet, fmt.Sprintf("/api/system/v2/tenants/%d", id), nil, &out)
	return out, err
}

// PatchTenant partially updates a tenant/system config record — patch is
// whatever subset of the v2 body shape should change (build it as a
// map[string]any of only the fields being touched; anything left out is
// left alone by PBXware, not reset to a default — see ClientV2's doc
// comment). Returns the tenant's full config as PBXware reports it after
// the update.
func (c *ClientV2) PatchTenant(id int, patch map[string]any) (TenantConfigV2, error) {
	var out TenantConfigV2
	err := c.call(http.MethodPatch, fmt.Sprintf("/api/system/v2/tenants/%d", id), patch, &out)
	return out, err
}

// TenantCreateParamsV2 holds what SwarmDialer sets when creating a tenant
// via v2's true one-call create (see CreateTenant) — unlike v1's
// AddTenant (bare identity fields only, needing a separate ResaveTenant
// afterward to raise channel limits/codecs), v2 accepts channel limits
// and the codec allowlist in the same create request.
type TenantCreateParamsV2 struct {
	Name              string
	Code              int // v2 wants this as a JSON number; v1's tenant_code is a string
	PackageID         int
	ExtLength         int
	CountryID         int
	NationalCode      string
	InternationalCode string
	// ChannelLimit is applied to every one of the tenant's channel-like
	// concurrency pools (local, remote, conferences, queues, erg, ivr,
	// dahdi) — confirmed live for v1 (see Client.ResaveTenant's doc
	// comment): PBXware's local-dialing AGI is gated by several of these
	// even for a plain two-extension call, not just local/remote: raising
	// only local/remote leaves calls hitting a hard wall at the default of
	// 8. v2's schema has the same seven fields under channels_limit (its
	// "ivr" corresponds to v1's "aach"/Auto Attendants), so all seven are
	// set here too.
	ChannelLimit int
	Codecs       []string // applied to both the local and remote allowlists
}

// CreateTenant creates a tenant with v2's true one-call create. Confirmed
// live 2026-09-25 on the redeployed (faster) hardware: 201 in ~8s, and
// extensions created right after it registered and called each other with
// no settling wait or resave — the >60s daemon timeout seen on the old
// hardware was the hardware, not v2. Returns the created tenant's config.
func (c *ClientV2) CreateTenant(p TenantCreateParamsV2) (TenantConfigV2, error) {
	body := map[string]any{
		"name":       p.Name,
		"code":       p.Code,
		"package_id": p.PackageID,
		"locality": map[string]any{
			"country_id":         p.CountryID,
			"national_code":      p.NationalCode,
			"international_code": p.InternationalCode,
		},
		"numbering_defaults": map[string]any{
			"ext_length": p.ExtLength,
		},
		"channels_limit": map[string]any{
			"local":       p.ChannelLimit,
			"remote":      p.ChannelLimit,
			"conferences": p.ChannelLimit,
			"queues":      p.ChannelLimit,
			"erg":         p.ChannelLimit,
			"ivr":         p.ChannelLimit,
			"dahdi":       p.ChannelLimit,
		},
		"codecs": map[string]any{
			"local":  p.Codecs,
			"remote": p.Codecs,
		},
	}
	var out TenantConfigV2
	err := c.call(http.MethodPost, "/api/system/v2/tenants", body, &out)
	return out, err
}

// TenantCode formats a Multi-Tenant tenant code the way v2's org paths
// want it (/api/org/{tenant}/v2/...): always three digits.
func TenantCode(code int) string { return fmt.Sprintf("%03d", code) }

// DefaultOrg is the org path segment for a non-Multi-Tenant edition, which
// has no tenant codes.
const DefaultOrg = "default"

// ListTenants lists every tenant record, including the SystemTenantID one.
// v2's tenant list is paginated; SwarmDialer only ever needs to find its
// own tenant among a handful, so a single large page is enough.
func (c *ClientV2) ListTenants() ([]TenantConfigV2, error) {
	var out struct {
		Data []TenantConfigV2 `json:"data"`
	}
	err := c.call(http.MethodGet, "/api/system/v2/tenants?size=1000", nil, &out)
	return out.Data, err
}

// FindTenantByCode returns the tenant with the given code, or ok=false if
// there is none.
func (c *ClientV2) FindTenantByCode(code int) (TenantConfigV2, bool, error) {
	tenants, err := c.ListTenants()
	if err != nil {
		return TenantConfigV2{}, false, err
	}
	for _, t := range tenants {
		if t.ID != SystemTenantID && t.Code == code {
			return t, true, nil
		}
	}
	return TenantConfigV2{}, false, nil
}

// DeleteTenant deletes a tenant along with its extensions. Synchronous on
// v2 (204 in ~2.5s, confirmed live 2026-09-25), unlike v1's tenant.delete.
func (c *ClientV2) DeleteTenant(id int) error {
	return c.call(http.MethodDelete, fmt.Sprintf("/api/system/v2/tenants/%d", id), nil, nil)
}

// swarmDialerPackageBodyV2 is SwarmDialer's fixed tenant package profile
// in v2's shape — same profile as v1's packageParams (see its doc comment
// for why): generous limits everywhere, call recording on, everything else
// optional off. v2 additionally validates that voicemails >= extensions.
func swarmDialerPackageBodyV2() map[string]any {
	return map[string]any{
		"name":                   SwarmDialerPackageName,
		"extensions":             swarmDialerPackageLimit,
		"voicemails":             swarmDialerPackageLimit,
		"queues":                 swarmDialerPackageLimit,
		"ivrs":                   swarmDialerPackageLimit,
		"conferences":            swarmDialerPackageLimit,
		"ring_groups":            swarmDialerPackageLimit,
		"hot_desking":            swarmDialerPackageLimit,
		"restrict_service_plans": "no",
		"call_recordings":        "yes",
		"call_monitoring":        "no",
		"call_screening":         "no",
	}
}

type packageV2 struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// FindSwarmDialerPackage returns the ID of the SwarmDialerPackageName
// package, or ok=false if there is none.
func (c *ClientV2) FindSwarmDialerPackage() (int, bool, error) {
	var out struct {
		Data []packageV2 `json:"data"`
	}
	if err := c.call(http.MethodGet, "/api/system/v2/tenant-packages?size=1000", nil, &out); err != nil {
		return 0, false, err
	}
	for _, p := range out.Data {
		if p.Name == SwarmDialerPackageName {
			return p.ID, true, nil
		}
	}
	return 0, false, nil
}

// EnsureSwarmDialerPackage returns the ID of the SwarmDialerPackageName
// package, creating it if missing or re-applying the current profile to it
// if it already exists (PUT — a full replace, which is fine here since the
// whole profile is sent). Same "always re-apply" approach as v1's
// Client.EnsureSwarmDialerPackage.
func (c *ClientV2) EnsureSwarmDialerPackage() (int, error) {
	id, ok, err := c.FindSwarmDialerPackage()
	if err != nil {
		return 0, fmt.Errorf("listing tenant packages: %w", err)
	}
	if ok {
		if err := c.call(http.MethodPut, fmt.Sprintf("/api/system/v2/tenant-packages/%d", id), swarmDialerPackageBodyV2(), nil); err != nil {
			return 0, fmt.Errorf("updating tenant package %d: %w", id, err)
		}
		return id, nil
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := c.call(http.MethodPost, "/api/system/v2/tenant-packages", swarmDialerPackageBodyV2(), &out); err != nil {
		return 0, fmt.Errorf("creating tenant package: %w", err)
	}
	return out.ID, nil
}

// DeletePackage deletes a tenant package. Fails if a tenant still uses it.
func (c *ClientV2) DeletePackage(id int) error {
	return c.call(http.MethodDelete, fmt.Sprintf("/api/system/v2/tenant-packages/%d", id), nil, nil)
}

// ExtensionCreateV2 is what SwarmDialer sets per extension on v2's create.
// Everything else is left to PBXware's defaults (see GET
// /extensions/default).
type ExtensionCreateV2 struct {
	Number        int
	Name          string
	Email         string
	Secret        string // SIP registration secret
	UserPassword  string // client-app login password; required by v2, unused by SwarmDialer
	PIN           string
	UADID         int // 50 = Generic SIP
	IncomingLimit int // v2 rejects > 999, same as v1
	OutgoingLimit int
	Codecs        []string
}

func (e ExtensionCreateV2) body() map[string]any {
	codecs := make([]map[string]any, 0, len(e.Codecs))
	for _, name := range e.Codecs {
		codecs = append(codecs, map[string]any{"name": name})
	}
	return map[string]any{
		"number":    e.Number,
		"name":      e.Name,
		"email":     e.Email,
		"status":    "active",
		"dtmf_mode": "rfc2833",
		"user_type": "friend",
		"authentication": map[string]any{
			"secret":        e.Secret,
			"user_password": e.UserPassword,
			"pin":           e.PIN,
		},
		"uad":          map[string]any{"id": e.UADID, "location": "local"},
		"call_control": map[string]any{"incoming_limit": e.IncomingLimit, "outgoing_limit": e.OutgoingLimit},
		"codecs":       map[string]any{"allowed": codecs},
	}
}

// BatchItemResultV2 is one item of a batch extension create's response.
// ID echoes the request item's correlation ID (SwarmDialer uses the
// extension number); Status is that item's own HTTP-style status. On
// success (201) ExtensionID is set; otherwise Err holds the item's error.
type BatchItemResultV2 struct {
	ID          int
	Status      int
	ExtensionID int
	Err         *v2APIError
}

// CreateExtensionsBatch creates several extensions in one request, each
// succeeding or failing independently (one reserved number doesn't fail
// the rest). Confirmed live 2026-09-25: 500 extensions in ~20s.
//
// Two differences from Bicom's published docs, both confirmed live: each
// request item's "id" must be an integer (the docs show a string like
// "req-1", which fails the whole request with 400 "invalid request body"),
// and the response is a bare JSON array rather than {"responses": [...]}.
func (c *ClientV2) CreateExtensionsBatch(org string, exts []ExtensionCreateV2) ([]BatchItemResultV2, error) {
	reqs := make([]map[string]any, 0, len(exts))
	for _, e := range exts {
		reqs = append(reqs, map[string]any{"id": e.Number, "data": e.body()})
	}
	var raw []struct {
		ID     int             `json:"id"`
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if err := c.call(http.MethodPost, fmt.Sprintf("/api/org/%s/v2/extensions/batch", org), map[string]any{"requests": reqs}, &raw); err != nil {
		return nil, err
	}
	out := make([]BatchItemResultV2, 0, len(raw))
	for _, r := range raw {
		item := BatchItemResultV2{ID: r.ID, Status: r.Status}
		if r.Status >= 200 && r.Status < 300 {
			var created struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(r.Body, &created); err != nil {
				return nil, fmt.Errorf("decoding batch item %d: %w", r.ID, err)
			}
			item.ExtensionID = created.ID
		} else {
			item.Err = &v2APIError{}
			if json.Unmarshal(r.Body, item.Err) != nil || item.Err.Description == "" {
				item.Err = &v2APIError{Code: r.Status, Description: string(r.Body)}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// ExtensionV2 is the subset of a v2 extension record SwarmDialer reads
// back — mainly the SIP username, which PBXware derives itself (tenant
// code + number on Multi-Tenant, just the number on other editions).
type ExtensionV2 struct {
	ID             int    `json:"id"`
	Number         int    `json:"number"`
	Name           string `json:"name"`
	Authentication struct {
		Username int    `json:"username"`
		Secret   string `json:"secret"`
	} `json:"authentication"`
}

// GetExtensionByNumber reads one extension by its number.
func (c *ClientV2) GetExtensionByNumber(org string, number int) (ExtensionV2, error) {
	var out ExtensionV2
	err := c.call(http.MethodGet, fmt.Sprintf("/api/org/%s/v2/extensions/number/%d", org, number), nil, &out)
	return out, err
}

// DeleteExtension deletes one extension by its ID.
func (c *ClientV2) DeleteExtension(org string, id int) error {
	return c.call(http.MethodDelete, fmt.Sprintf("/api/org/%s/v2/extensions/%d", org, id), nil, nil)
}

type trunkCodecV2 struct {
	Name  string `json:"name"`
	Ptime int    `json:"ptime"`
}

// GetTrunkCodecs returns a trunk's allowed codecs, in its preference order.
func (c *ClientV2) GetTrunkCodecs(id int) ([]string, error) {
	var out struct {
		Codecs struct {
			Allowed []trunkCodecV2 `json:"allowed"`
		} `json:"codecs"`
	}
	if err := c.call(http.MethodGet, fmt.Sprintf("/api/system/v2/trunks/%d", id), nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Codecs.Allowed))
	for _, a := range out.Codecs.Allowed {
		names = append(names, a.Name)
	}
	return names, nil
}

// SetTrunkCodecs replaces a trunk's allowed codecs with codecs, in that
// preference order (20ms ptime each). A true partial PATCH: only the codec
// list changes (confirmed live 2026-09-26 by diffing the full trunk config
// before and after).
func (c *ClientV2) SetTrunkCodecs(id int, codecs []string) error {
	allowed := make([]trunkCodecV2, 0, len(codecs))
	for _, name := range codecs {
		allowed = append(allowed, trunkCodecV2{Name: name, Ptime: 20})
	}
	return c.call(http.MethodPatch, fmt.Sprintf("/api/system/v2/trunks/%d", id),
		map[string]any{"codecs": map[string]any{"allowed": allowed}}, nil)
}

// TrunkCodecOrder returns SwarmDialerCodecsList with first moved to the
// front — the trunk order a remote batch using codec first needs (see
// cmd/gui's prepareRemoteBatch).
func TrunkCodecOrder(first string) []string {
	order := []string{first}
	for _, c := range SwarmDialerCodecsList {
		if c != first {
			order = append(order, c)
		}
	}
	return order
}
