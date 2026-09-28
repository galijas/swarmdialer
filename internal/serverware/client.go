// Package serverware talks to a SERVERware controller: its REST API (VPSs,
// hosts, settings) and the Prometheus it runs (host and VPS metrics).
// Both accept the same API token, sent as the SW-API-Token header.
//
// See docs/serverware_api_reference.md for what was confirmed live,
// including two calls missing from SERVERware's published OpenAPI spec
// (the observability setting, and the VPS collect_metrics field).
package serverware

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is one SERVERware controller.
type Client struct {
	BaseURL string // e.g. "https://10.1.101.10"
	Token   string
	http    *http.Client
}

// NewClient builds a Client. controller may be a bare IP/host name or a
// full URL. Controllers commonly use a self-signed certificate, so it's
// not verified — same allowance as the PBXware clients.
func NewClient(controller, token string) *Client {
	base := strings.TrimRight(strings.TrimSpace(controller), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return &Client{
		BaseURL: base,
		Token:   strings.TrimSpace(token),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

// APIError is a non-success response from the SERVERware API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("SERVERware API error %d: %s", e.Status, e.Message)
}

// IsUnauthorized reports whether err means the API token was rejected.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("SW-API-Token", c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=utf-8")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling SERVERware %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		// Error bodies are {"Error": "..."}; fall back to the raw body.
		var e struct {
			Error string `json:"Error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return raw, nil
}

// call performs an API request whose success body is {"data": ..., "status": "success"}.
func (c *Client) call(method, path string, body, out any) error {
	raw, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decoding SERVERware response for %s: %w", path, err)
	}
	return json.Unmarshal(env.Data, out)
}

// Host is the subset of a SERVERware host record SwarmDialer uses.
type Host struct {
	ID        int    `json:"id"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ip_address"`
	Purpose   string `json:"purpose"`
	MirrorID  int    `json:"mirror_id"`
	State     string `json:"state"`
	// PlatformDetails describe the host's hardware, as shown in the GUI's
	// host details (on Mirror: the active node's).
	PlatformDetails PlatformDetails `json:"platform_details"`
}

// PlatformDetails is the hardware subset of a host's platform_details.
type PlatformDetails struct {
	CPUModel    string `json:"cpu_model"` // e.g. "Intel(R) Xeon(R) Silver 4208 CPU @ 2.10GHz"
	CPUCount    int    `json:"cpu_count"` // logical CPUs
	Motherboard string `json:"motherboard"`
}

// Hosts lists the network's hosts. Also the cheapest call to check that
// the API token works.
func (c *Client) Hosts() ([]Host, error) {
	var out []Host
	err := c.call(http.MethodGet, "/api/networks/1/hosts", nil, &out)
	return out, err
}

// VPS is the subset of a VPS record SwarmDialer uses. IPAddresses is only
// filled in by the list call (VPSes), not the single-VPS read.
type VPS struct {
	ID             int      `json:"id"`
	UUID           string   `json:"uuid"`
	HostID         int      `json:"host_id"`
	Name           string   `json:"name"`
	Engine         string   `json:"engine"` // "lxc" or "kvm"
	State          string   `json:"state"`
	Task           string   `json:"task"`
	IPAddresses    []string `json:"ip_addresses"`
	CollectMetrics bool     `json:"collect_metrics"`
	CPULimit       int      `json:"cpu_limit"`
	CPUShare       int      `json:"cpu_share"`
	MemLimitMB     int      `json:"mem_limit"`
	CallrecRAMMB   int      `json:"callrec_ram_mb"`
	PBXwareVersion string   `json:"pbxware_version"` // PBXware VPSs only, e.g. "8.2.0.0 "
}

// VPSes lists every VPS on the network. Note: its collect_metrics values
// were seen stale (false for VPSs that had it on); read one VPS with GetVPS
// for that field.
func (c *Client) VPSes() ([]VPS, error) {
	var out []VPS
	err := c.call(http.MethodGet, "/api/networks/1/vpses", nil, &out)
	return out, err
}

// GetVPS reads one VPS.
func (c *Client) GetVPS(id int) (VPS, error) {
	var out VPS
	err := c.call(http.MethodGet, fmt.Sprintf("/api/networks/1/vpses/%d", id), nil, &out)
	return out, err
}

// EnableCollectMetrics turns on per-VPS metrics collection ("Collect
// Metrics" in the Edit VPS form) and waits for SERVERware to finish
// applying it. It uses the documented Edit VPS call, which replaces the
// whole VPS record: the current record is read and sent back with only
// collect_metrics changed, and root_password empty (which leaves the
// password unchanged, as the GUI does). Confirmed live 2026-09-28 on an
// LXC PBXware VPS: only collect_metrics changed and the VPS kept running.
func (c *Client) EnableCollectMetrics(id int, wait time.Duration) error {
	var rec map[string]any
	if err := c.call(http.MethodGet, fmt.Sprintf("/api/networks/1/vpses/%d", id), nil, &rec); err != nil {
		return fmt.Errorf("reading VPS %d: %w", id, err)
	}
	if on, _ := rec["collect_metrics"].(bool); on {
		return nil
	}
	rec["collect_metrics"] = true
	rec["root_password"] = ""
	if _, err := c.do(http.MethodPut, fmt.Sprintf("/api/networks/1/vpses/%d", id), rec); err != nil {
		return fmt.Errorf("editing VPS %d: %w", id, err)
	}
	deadline := time.Now().Add(wait)
	for {
		v, err := c.GetVPS(id)
		if err == nil && v.Task == "" && v.CollectMetrics {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("waiting for VPS %d to apply collect_metrics: %w", id, err)
			}
			return fmt.Errorf("VPS %d still hasn't applied collect_metrics after %s (task %q)", id, wait, v.Task)
		}
		time.Sleep(2 * time.Second)
	}
}

// ObservabilityEnabled reads the system-wide observability setting
// (System Settings -> Observability in the GUI). Not in SERVERware's
// published spec; confirmed live 2026-09-28.
func (c *Client) ObservabilityEnabled() (bool, error) {
	var out struct {
		Observability struct {
			Enabled bool `json:"enabled"`
		} `json:"observability"`
	}
	err := c.call(http.MethodGet, "/api/system-settings/observability", nil, &out)
	return out.Observability.Enabled, err
}

// EnableObservability turns observability on (it starts the SRW exporter
// on the controller). Not in the published spec; confirmed live
// 2026-09-28.
func (c *Client) EnableObservability() error {
	_, err := c.do(http.MethodPost, "/api/system-settings", map[string]any{
		"observability": map[string]any{"enabled": true},
	})
	return err
}

// --- Prometheus ---

// PromTarget is one Prometheus scrape target.
type PromTarget struct {
	Job      string
	Instance string
	Port     string
	Health   string // "up" or "down"
}

// PromTargets lists Prometheus's active scrape targets.
func (c *Client) PromTargets() ([]PromTarget, error) {
	raw, err := c.do(http.MethodGet, "/prometheus/api/v1/targets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			ActiveTargets []struct {
				Labels    map[string]string `json:"labels"`
				ScrapeURL string            `json:"scrapeUrl"`
				Health    string            `json:"health"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding Prometheus targets: %w", err)
	}
	targets := make([]PromTarget, 0, len(out.Data.ActiveTargets))
	for _, t := range out.Data.ActiveTargets {
		port := ""
		if u, err := url.Parse(t.ScrapeURL); err == nil {
			port = u.Port()
		}
		targets = append(targets, PromTarget{Job: t.Labels["job"], Instance: t.Labels["instance"], Port: port, Health: t.Health})
	}
	return targets, nil
}

// PromSample is one instant-query result.
type PromSample struct {
	Labels map[string]string
	Value  float64
}

// PromQuery runs an instant PromQL query.
func (c *Client) PromQuery(query string) ([]PromSample, error) {
	raw, err := c.do(http.MethodGet, "/prometheus/api/v1/query?query="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding Prometheus query result: %w", err)
	}
	samples := make([]PromSample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		var v float64
		if s, ok := r.Value[1].(string); ok {
			fmt.Sscanf(s, "%g", &v)
		}
		samples = append(samples, PromSample{Labels: r.Metric, Value: v})
	}
	return samples, nil
}
