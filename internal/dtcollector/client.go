// Package dtcollector uploads finished test reports to DT Collector, the
// central report server. The API contract is in ~/claude/DTcollector_project.md
// (the separate DT Collector project): GET /api/v1/ping to check an upload
// key, POST /api/v1/reports to upload.
//
// Unlike the PBXware and SERVERware clients, this one verifies the
// server's TLS certificate: DT Collector is public and uses a real
// (Let's Encrypt) certificate, and the upload key must not be sent to an
// impostor.
package dtcollector

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is the central DT Collector every SwarmDialer uploads to
// unless Settings overrides it. The upload key is never built in: it's
// created in DT Collector's interface and entered per deployment.
const DefaultURL = "https://dtcollector.dtbicom.xyz"

// URLOrDefault returns url, or DefaultURL if url is empty.
func URLOrDefault(url string) string {
	if url == "" {
		return DefaultURL
	}
	return url
}

// Client is one DT Collector server.
type Client struct {
	BaseURL string
	Key     string
	http    *http.Client
}

// NewClient builds a Client. baseURL may omit the scheme (https is used).
func NewClient(baseURL, key string) *Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = "https://" + base
	}
	return &Client{BaseURL: base, Key: strings.TrimSpace(key), http: &http.Client{Timeout: 30 * time.Second}}
}

// ErrInvalidKey means DT Collector rejected the upload key.
var ErrInvalidKey = errors.New("DT Collector rejected the upload key")

// Ping checks the upload key.
func (c *Client) Ping() error {
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return errors.New("DT Collector URL must use https")
	}
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/v1/ping", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching DT Collector: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrInvalidKey
	default:
		return fmt.Errorf("DT Collector ping returned HTTP %d", resp.StatusCode)
	}
}

// UploadResult is DT Collector's answer to an upload.
type UploadResult struct {
	ID        string `json:"id"`
	Duplicate bool   `json:"duplicate"` // it already had this report (a retried upload)
}

// PermanentError is an upload DT Collector rejected for a reason retrying
// won't fix (invalid key, invalid report, too large).
type PermanentError struct{ msg string }

func (e *PermanentError) Error() string { return e.msg }

// Upload sends one report (gzip-compressed JSON). Uploads are idempotent
// on DT Collector's side (by report_id), so a failed upload can simply be
// retried.
func (c *Client) Upload(report any) (UploadResult, error) {
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return UploadResult{}, &PermanentError{"DT Collector URL must use https"}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(report); err != nil {
		return UploadResult{}, err
	}
	if err := gz.Close(); err != nil {
		return UploadResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v1/reports", &buf)
	if err != nil {
		return UploadResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return UploadResult{}, fmt.Errorf("reaching DT Collector: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var out struct {
		UploadResult
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return out.UploadResult, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return UploadResult{}, &PermanentError{ErrInvalidKey.Error()}
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return UploadResult{}, &PermanentError{fmt.Sprintf("DT Collector rejected the report (HTTP %d): %s", resp.StatusCode, msg)}
	default:
		return UploadResult{}, fmt.Errorf("DT Collector returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
