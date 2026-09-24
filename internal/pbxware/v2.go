package pbxware

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// Bicom 2026-09-24) — this client only covers what v1 can't do safely as a
// partial update: system/tenant-level channel limits, codecs, and
// call-recording settings. Everything else (tenants, extensions, trunks,
// DIDs, packages) stays on v1's Client.
type ClientV2 struct {
	BaseURL string // e.g. "https://10.1.100.208"
	APIKey  string // sent as "Authorization: Bearer <APIKey>"
	http    *http.Client
}

// NewClientV2 builds a ClientV2. Same self-signed-cert allowance as
// NewClient — fine for internal load-testing use only.
func NewClientV2(baseURL, apiKey string) *ClientV2 {
	return &ClientV2{
		BaseURL: baseURL,
		APIKey:  apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

// v2APIError is v2's error response shape — {"code": N, "description": "..."}
// — confirmed live for both a validation failure (400) and a bad token
// (401), consistently, unlike v1's ad hoc {"error": "..."} sometimes mixed
// into an otherwise-successful-shaped body.
type v2APIError struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
}

func (e *v2APIError) Error() string {
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

// TenantConfigV2 is the subset of a v2 tenant/system config record this
// project reads — PBXware's actual response has many more fields, which
// encoding/json silently ignores on decode.
type TenantConfigV2 struct {
	ID             int              `json:"id"`
	Name           string           `json:"name"`
	ChannelsLimit  ChannelsLimitV2  `json:"channels_limit"`
	Codecs         CodecsV2         `json:"codecs"`
	CallRecordings CallRecordingsV2 `json:"call_recordings"`
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
