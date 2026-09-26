// Package wizard implements the Setup Wizard's backend logic: connecting
// to a PBXware instance, provisioning a tenant + extensions on it (with
// live progress), and connecting two servers with a trunk + one DID per
// extension. See docs/gui_spec.md for the full spec this implements.
package wizard

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/netutil"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// SIPHostFrom derives the SIP signaling target (host:port) from a PBXware
// base URL like "https://10.1.100.208" — same host, standard SIP port.
// PBXware's HTTP API and SIP signaling share the same host in every setup
// we've tested; there's no separate field for this in the wizard spec.
func SIPHostFrom(baseURL string) string {
	host := baseURL
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(host, prefix) {
			host = host[len(prefix):]
			break
		}
	}
	host = strings.TrimSuffix(host, "/")
	return host + ":5060"
}

// ConnectionResult is what TestConnection returns on success.
type ConnectionResult struct {
	License pbxware.LicenseInfo
	LocalIP string // auto-detected — see netutil.DetectLocalIP
}

// TestConnection validates a PBXware base URL + both API keys (legacy v1
// and v2 — every instance needs both now, see ProvisionParams) by making a
// lightweight read call on each (v1's license.info, v2's GetTenant against
// SystemTenantID) and detects the local IP to advertise for SIP on this
// connection. PBXware API keys appear to be all-or-nothing admin keys (no
// separate read/write scoping observed), so a successful read is treated
// as sufficient confirmation of write access too — see docs/gui_spec.md's
// open-question note on this.
func TestConnection(baseURL, apiKey, apiKeyV2 string) (ConnectionResult, error) {
	client := pbxware.NewClient(baseURL, apiKey)
	license, err := client.GetLicenseInfo()
	if err != nil {
		return ConnectionResult{}, fmt.Errorf("connecting to %s (legacy API key): %w", baseURL, err)
	}
	clientV2 := pbxware.NewClientV2(baseURL, apiKeyV2)
	if _, err := clientV2.GetTenant(pbxware.SystemTenantID); err != nil {
		return ConnectionResult{}, fmt.Errorf("connecting to %s (API v2 key): %w", baseURL, err)
	}
	localIP, err := netutil.DetectLocalIP(SIPHostFrom(baseURL))
	if err != nil {
		return ConnectionResult{}, fmt.Errorf("detecting local IP for %s: %w", baseURL, err)
	}
	return ConnectionResult{License: license, LocalIP: localIP}, nil
}

// ProvisionProgress reports live progress of a provisioning run — read it
// concurrently (e.g. from an HTTP handler) via Snapshot while Provision
// runs in the background.
type ProvisionProgress struct {
	mu      sync.Mutex
	total   int
	created int
	message string
	done    bool
	err     string
	warning string // non-fatal: shown to the user once provisioning is done
}

func (p *ProvisionProgress) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// ProvisionProgressSnapshot is a point-in-time, serializable copy of a
// ProvisionProgress — deliberately a separate type (not a copy of
// ProvisionProgress itself), since that struct embeds a sync.Mutex and
// copying it is a real bug (caught by `go vet`'s copylocks check).
type ProvisionProgressSnapshot struct {
	Total   int    `json:"total"`
	Created int    `json:"created"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Err     string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// Snapshot returns a copy safe to read or serialize without locking.
func (p *ProvisionProgress) Snapshot() ProvisionProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProvisionProgressSnapshot{Total: p.total, Created: p.created, Message: p.message, Done: p.done, Err: p.err, Warning: p.warning}
}

// ProvisionParams configures one server's provisioning step.
type ProvisionParams struct {
	Name           string // display name for this server
	BaseURL        string
	APIKey         string // legacy (v1) API key
	APIKeyV2       string
	ExtensionCount int
	// Tenant creation fields — ignored (and tenant creation skipped) if
	// the license's edition isn't Multi-Tenant. Package is not a field
	// here — Multi-Tenant editions require an existing package to create
	// a tenant against, so Provision ensures its own (see
	// pbxware.EnsureSwarmDialerPackage) rather than taking one as input.
	TenantCode    string
	TenantName    string
	ExtLength     int
	Country       string
	National      string
	International string
	ChannelLimit  int // tenant channel limit (PBXware defaults to 8); per-extension limits are capped at 999
	MaxWait       time.Duration
}

// Provision creates (or, for non-Multi-Tenant editions, skips) a tenant
// and ExtensionCount extensions on it, reporting progress via progress
// (pass a fresh &ProvisionProgress{} and poll Snapshot from elsewhere
// while this runs — it blocks until done or failed). Returns a
// store.Server ready to be saved.
func Provision(ctx context.Context, p ProvisionParams, progress *ProvisionProgress) (*store.Server, error) {
	progress.set(func() { progress.total = p.ExtensionCount; progress.message = "connecting" })

	client := pbxware.NewClient(p.BaseURL, p.APIKey)
	clientV2 := pbxware.NewClientV2(p.BaseURL, p.APIKeyV2)

	license, err := client.GetLicenseInfo()
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("checking license: %w", err)
	}

	localIP, err := netutil.DetectLocalIP(SIPHostFrom(p.BaseURL))
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("detecting local IP: %w", err)
	}

	isMultiTenant := license.IsMultiTenant()
	srv := &store.Server{
		ID:       fmt.Sprintf("srv-%d", time.Now().UnixNano()),
		Name:     p.Name,
		BaseURL:  p.BaseURL,
		APIKey:   p.APIKey,
		APIKeyV2: p.APIKeyV2,
		Edition:  license.Edition,
		SIPHost:  SIPHostFrom(p.BaseURL),
		LocalIP:  localIP,
	}

	// System-level config (see pbxware.SystemTenantID), on every edition:
	// RAM disk is required for stereo call recording, which is always
	// system-level; the system-wide channel limit (PBXware defaults it to
	// 246 regardless of license) and codec allowlist are raised too, since
	// on Multi-Tenant the system record's own values are also at those
	// defaults and would otherwise sit above every tenant as a cap. Done
	// first, before any tenant/extension exists: right after this PATCH
	// PBXware briefly rejected a fresh registration with 403 (confirmed
	// live 2026-09-25, cleared within seconds), and doing it here keeps
	// that window inside provisioning rather than at the first dial.
	progress.set(func() { progress.message = "raising system-level channels/codecs/recording settings" })
	before, err := clientV2.GetTenant(pbxware.SystemTenantID)
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("reading system-level settings via API v2: %w", err)
	}
	systemPatch := map[string]any{
		"call_recordings": map[string]any{
			"use_ram_disk":  "yes",
			"ram_disk_size": ramDiskSizeMB,
		},
		"channels_limit": map[string]any{"local": systemChannelLimit, "remote": systemChannelLimit},
		"codecs": map[string]any{
			"local":  pbxware.SwarmDialerCodecsList,
			"remote": pbxware.SwarmDialerCodecsList,
		},
	}
	if _, err := clientV2.PatchTenant(pbxware.SystemTenantID, systemPatch); err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, fmt.Errorf("raising system-level settings via API v2: %w", err)
	}
	// PBXware runs in an LXC container on SERVERware, where its start script
	// skips mounting the recording RAM disk: the tmpfs comes from the
	// container's config on the SERVERware host, so the size set above
	// doesn't change it (confirmed live 2026-09-26: still 64MB after a
	// PBXware restart; 512MB only after raising it in SERVERware and
	// restarting the VPS). There's no API for either, so tell the user —
	// only when this run actually changed the setting.
	if before.CallRecordings.UseRAMDisk != "yes" || before.CallRecordings.RAMDiskSize != ramDiskSizeMB {
		progress.set(func() {
			progress.warning = fmt.Sprintf("Call recording RAM disk on %s was set to %dMB in PBXware, but the actual RAM disk is provided by SERVERware. "+
				"Increase this VPS's recording RAM disk size to %dMB in SERVERware, then restart the VPS. Until then it stays at its old size (typically 64MB), "+
				"which large or stereo recording batches can fill.", p.Name, ramDiskSizeMB, ramDiskSizeMB)
		})
	}

	// org is the v2 path segment extensions are created under: the
	// tenant's 3-digit code on Multi-Tenant, "default" otherwise.
	org := pbxware.DefaultOrg
	extDigits := p.ExtLength // Multi-Tenant: our own free choice, set on the tenant below
	if isMultiTenant {
		countryID, err := strconv.Atoi(p.Country)
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("parsing country %q as a numeric ID: %w", p.Country, err)
		}
		code, err := strconv.Atoi(p.TenantCode)
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("parsing tenant code %q as a number: %w", p.TenantCode, err)
		}

		progress.set(func() { progress.message = "ensuring tenant package exists" })
		packageID, err := clientV2.EnsureSwarmDialerPackage()
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("ensuring tenant package: %w", err)
		}

		// v2's create sets channel limits and the codec allowlist in the
		// same call, so the old v1 AddTenant + ResaveTenant (x2) sequence
		// is gone entirely — confirmed live 2026-09-25 that extensions
		// created straight after it register and call with no resave.
		progress.set(func() { progress.message = "creating tenant" })
		tenantCfg, err := clientV2.CreateTenant(pbxware.TenantCreateParamsV2{
			Name: p.TenantName, Code: code, PackageID: packageID,
			ExtLength: p.ExtLength, CountryID: countryID,
			NationalCode: p.National, InternationalCode: p.International,
			ChannelLimit: p.ChannelLimit, Codecs: pbxware.SwarmDialerCodecsList,
		})
		if err != nil {
			// A create that outlasts our HTTP timeout can still succeed
			// server-side (seen on the old, slower hardware), so check for
			// it by code before giving up.
			found, ok, listErr := clientV2.FindTenantByCode(code)
			if listErr != nil || !ok {
				progress.set(func() { progress.done = true; progress.err = err.Error() })
				return nil, fmt.Errorf("creating tenant: %w", err)
			}
			tenantCfg = found
		}

		srv.TenantID = tenantCfg.ID
		srv.TenantCode = p.TenantCode
		org = pbxware.TenantCode(code)
	} else {
		progress.set(func() { progress.message = "detecting system extension number length" })
		// Non-Multi-Tenant editions have one system-wide extension digit
		// length we must detect and match, not choose ourselves — every
		// extension of any other length is rejected. Confirmed live
		// (2026-09-18) against a system configured for 4-digit extensions.
		system, err := clientV2.GetTenant(pbxware.SystemTenantID)
		if err != nil {
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, fmt.Errorf("detecting system extension length: %w", err)
		}
		extDigits = system.NumberingDefaults.ExtLength
		if extDigits <= 0 {
			err := fmt.Errorf("system reports no extension length (numbering_defaults.ext_length=%d)", extDigits)
			progress.set(func() { progress.done = true; progress.err = err.Error() })
			return nil, err
		}
	}

	extensions, err := provisionExtensions(ctx, clientV2, org, extDigits, p, progress)
	if err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, err
	}
	srv.Extensions = extensions

	progress.set(func() { progress.message = "waiting for PBXware to load the new extensions (SIP registration check)" })
	if err := waitUntilRegistrable(ctx, localIP, srv.SIPHost, extensions); err != nil {
		progress.set(func() { progress.done = true; progress.err = err.Error() })
		return nil, err
	}

	progress.set(func() { progress.done = true; progress.message = "done" })
	return srv, nil
}

// ramDiskSizeMB is the call recording RAM disk size set on every instance
// (PBXware's maximum), needed for stereo recording.
const ramDiskSizeMB = 512

// systemChannelLimit is the system-wide local/remote channel limit set on
// every instance.
const systemChannelLimit = 1000

// extensionBatchSize is how many extensions go into one v2 batch create.
// ~40ms per extension on the current hardware (500 took ~20s), so 100 per
// batch is ~4s: well inside the 30s client timeout, and frequent enough
// for live progress.
const extensionBatchSize = 100

// maxReservedSkips bounds how many numbers already taken by something else
// (e.g. a system's default Operator at the digit base, like 1000 —
// confirmed live 2026-09-18) are skipped before giving up. Far more than a
// handful suggests the whole range is in use.
const maxReservedSkips = 20

// provisionExtensions creates p.ExtensionCount extensions under org via v2
// batch create, numbered upward from the first extDigits-digit number,
// skipping numbers already taken by something else.
func provisionExtensions(ctx context.Context, clientV2 *pbxware.ClientV2, org string, extDigits int, p ProvisionParams, progress *ProvisionProgress) ([]pbxware.ProvisionedExtension, error) {
	extBase := 1
	for i := 1; i < extDigits; i++ {
		extBase *= 10
	}

	// A per-extension incoming/outgoing limit above 999 is rejected outright
	// ("'incoming_limit' must not be greater than 999") by both v1 and v2 —
	// stricter than the tenant/system-level channel limit, which does accept
	// 1000. This only caps each extension's own concurrency, not the
	// tenant's aggregate.
	extChannelLimit := p.ChannelLimit
	if extChannelLimit > 999 {
		extChannelLimit = 999
	}

	extensions := make([]pbxware.ProvisionedExtension, 0, p.ExtensionCount)
	usernamePrefix := ""
	prefixKnown := false
	candidate := extBase
	reservedSkips := 0

	for len(extensions) < p.ExtensionCount {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n := p.ExtensionCount - len(extensions)
		if n > extensionBatchSize {
			n = extensionBatchSize
		}
		batch := make([]pbxware.ExtensionCreateV2, 0, n)
		byNumber := make(map[int]pbxware.ExtensionCreateV2, n)
		for i := 0; i < n; i++ {
			secret, err := pbxware.GenerateSecret()
			if err != nil {
				return nil, fmt.Errorf("generating secret: %w", err)
			}
			userPassword, err := pbxware.GenerateSecret()
			if err != nil {
				return nil, fmt.Errorf("generating user password: %w", err)
			}
			e := pbxware.ExtensionCreateV2{
				Number:        candidate,
				Name:          fmt.Sprintf("SwarmDialer %d", candidate),
				Email:         fmt.Sprintf("swarmdialer%d@swarmdialer.local", candidate),
				Secret:        secret,
				UserPassword:  userPassword,
				PIN:           "1234",
				UADID:         50, // Generic SIP
				IncomingLimit: extChannelLimit,
				OutgoingLimit: extChannelLimit,
				Codecs:        pbxware.SwarmDialerCodecsList,
			}
			batch = append(batch, e)
			byNumber[candidate] = e
			candidate++
		}

		results, err := createBatchWithRetry(ctx, clientV2, org, batch, p.MaxWait)
		if err != nil {
			return nil, fmt.Errorf("creating extensions %d-%d: %w", batch[0].Number, batch[len(batch)-1].Number, err)
		}

		for _, r := range results {
			req, ok := byNumber[r.ID]
			if !ok {
				return nil, fmt.Errorf("batch create returned an unknown item id %d", r.ID)
			}
			var ext pbxware.ProvisionedExtension
			switch {
			case r.Err == nil:
				ext = pbxware.ProvisionedExtension{ExtensionID: r.ExtensionID, Ext: strconv.Itoa(req.Number), Secret: req.Secret}
			case pbxware.IsReservedExtensionError(r.Err) && isOwnExtension(r.Err, req.Number):
				// Our own extension from an earlier attempt of this same
				// batch that timed out client-side but went through on
				// PBXware's end — adopt it instead of skipping the number.
				existing, err := clientV2.GetExtensionByNumber(org, req.Number)
				if err != nil {
					return nil, fmt.Errorf("reading back already-created extension %d: %w", req.Number, err)
				}
				ext = pbxware.ProvisionedExtension{ExtensionID: existing.ID, Ext: strconv.Itoa(req.Number), Secret: existing.Authentication.Secret}
			case pbxware.IsReservedExtensionError(r.Err):
				reservedSkips++
				if reservedSkips > maxReservedSkips {
					return nil, fmt.Errorf("too many reserved-extension collisions from %d onward: %w", extBase, r.Err)
				}
				continue // taken by something else; doesn't count against ExtensionCount
			default:
				return nil, fmt.Errorf("creating extension %d: %w", req.Number, r.Err)
			}

			if !prefixKnown {
				// PBXware derives the SIP username itself (tenant code +
				// number on Multi-Tenant, just the number otherwise) and
				// the create response doesn't include it — read it back
				// once and reuse the prefix for every other extension.
				prefix, err := sipUsernamePrefix(clientV2, org, req.Number)
				if err != nil {
					return nil, err
				}
				usernamePrefix, prefixKnown = prefix, true
			}
			ext.Username = usernamePrefix + ext.Ext
			extensions = append(extensions, ext)
		}

		created := len(extensions)
		last := batch[len(batch)-1].Number
		progress.set(func() {
			progress.created = created
			progress.message = fmt.Sprintf("created extensions up to %d (%d/%d)", last, created, p.ExtensionCount)
		})
	}

	// Batch responses aren't guaranteed to be in request order.
	sort.Slice(extensions, func(i, j int) bool {
		a, _ := strconv.Atoi(extensions[i].Ext)
		b, _ := strconv.Atoi(extensions[j].Ext)
		return a < b
	})
	return extensions, nil
}

// createBatchWithRetry sends one batch, retrying the whole request with
// backoff on a request-level failure (transport error, timeout, 5xx) until
// maxWait. Per-item failures are not retried here — they come back in the
// results for the caller to handle. A retry after a timeout can find some
// of the batch already created; those come back as "reserved by
// SwarmDialer N" and the caller adopts them (see isOwnExtension).
func createBatchWithRetry(ctx context.Context, clientV2 *pbxware.ClientV2, org string, batch []pbxware.ExtensionCreateV2, maxWait time.Duration) ([]pbxware.BatchItemResultV2, error) {
	deadline := time.Now().Add(maxWait)
	backoff := 2 * time.Second
	for {
		results, err := clientV2.CreateExtensionsBatch(org, batch)
		if err == nil {
			return results, nil
		}
		if time.Now().Add(backoff).After(deadline) {
			return nil, fmt.Errorf("gave up after %s: %w", maxWait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// isOwnExtension reports whether a reserved-number error names the
// extension SwarmDialer itself would have created with that number.
func isOwnExtension(err error, number int) bool {
	return strings.Contains(err.Error(), fmt.Sprintf("reserved by (Extension) SwarmDialer %d", number))
}

// sipUsernamePrefix reads extension number back and returns the part of
// its SIP username that precedes the number.
func sipUsernamePrefix(clientV2 *pbxware.ClientV2, org string, number int) (string, error) {
	ext, err := clientV2.GetExtensionByNumber(org, number)
	if err != nil {
		return "", fmt.Errorf("reading back extension %d for its SIP username: %w", number, err)
	}
	username, num := strconv.Itoa(ext.Authentication.Username), strconv.Itoa(number)
	if !strings.HasSuffix(username, num) {
		return "", fmt.Errorf("extension %d has SIP username %q, which doesn't end in its number", number, username)
	}
	return strings.TrimSuffix(username, num), nil
}
