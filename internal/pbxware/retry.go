package pbxware

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// permanentExtensionErrors matches AddExtension error substrings that can
// never succeed no matter how long we wait, so retrying them is pure
// wasted time (up to the full maxWait) rather than a safety margin.
// Confirmed live (2026-09-18): a chosen extension number colliding with a
// pre-existing reserved one (e.g. a system's default Operator extension)
// fails with "extension is reserved" on every single attempt, and
// AddExtensionWithRetry burned the full 10-minute maxWait on it before
// this existed, indistinguishable from a real transient failure.
var permanentExtensionErrors = []string{
	"is reserved",
	"already exist",
	"contains invalid data",
}

func isPermanentExtensionError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range permanentExtensionErrors {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// IsReservedExtensionError reports whether err is specifically an
// extension-number collision (already in use / reserved for something
// else, e.g. a system's default Operator extension) — as opposed to some
// other permanent error. Callers choosing extension numbers themselves
// (see wizard.Provision) can use this to skip just that one number and
// try the next, rather than aborting the whole run over one collision.
func IsReservedExtensionError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "is reserved")
}

// AddExtensionWithRetry calls AddExtension, retrying with exponential backoff
// until it succeeds, maxWait elapses, or the error is one we know can never
// resolve itself (see permanentExtensionErrors) — failing those fast instead
// of burning the full retry window matters here because the caller
// (wizard.Provision) is placing one extension per loop iteration, and a
// permanent error on extension N should surface immediately, not after N
// has already blocked the whole run for minutes.
//
// For everything else, it deliberately retries on ANY error rather than
// trying to whitelist known "tenant not ready yet" error strings:
// empirically, a freshly created tenant's not-ready state has produced at
// least four different error shapes from PBXware ("Failed to generate
// configuration", "Table ... doesn't exist", a raw HTML error page instead
// of JSON, "Unable to create extension") and there's no reason to assume
// that list is complete. maxWait bounds the cost of this: an unrecognized
// permanent error (bad input, etc.) still surfaces correctly once the
// deadline is hit, just slower than an immediate failure would.
func (c *Client) AddExtensionWithRetry(p ExtensionParams, maxWait time.Duration) (ExtensionResult, error) {
	delay := 5 * time.Second
	const maxDelay = 60 * time.Second
	deadline := time.Now().Add(maxWait)

	for {
		result, err := c.AddExtension(p)
		if err == nil {
			return result, nil
		}
		if isPermanentExtensionError(err) {
			return ExtensionResult{}, fmt.Errorf("permanent error creating extension %s: %w", p.Ext, err)
		}
		if time.Now().Add(delay).After(deadline) {
			return ExtensionResult{}, fmt.Errorf("gave up after %s waiting for tenant provisioning: %w", maxWait, err)
		}
		time.Sleep(delay)
		delay = min(delay*2, maxDelay)
	}
}

// WaitForTenant blocks until a tenant appears ready to accept extensions
// (a harmless probe extension create-then-nothing isn't available, so this
// just waits for the tenant to show up in ListTenants — callers still need
// AddExtensionWithRetry for the slower per-tenant-DB provisioning step that
// follows).
func (c *Client) WaitForTenant(tenantID int, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for {
		tenants, err := c.ListTenants()
		if err != nil {
			return err
		}
		for _, t := range tenants {
			if t.ID == tenantID {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tenant %d did not appear within %s", tenantID, maxWait)
		}
		time.Sleep(5 * time.Second)
	}
}

// WaitForTenantGone blocks until tenantID no longer appears in
// ListTenants, or maxWait elapses — tenant deletion is just as slow as
// tenant creation (see WaitForTenant) on this system, and a caller that
// moves on immediately (e.g. to delete the shared package the tenant
// referenced) risks acting while the tenant delete is still in flight.
func (c *Client) WaitForTenantGone(tenantID int, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for {
		tenants, err := c.ListTenants()
		if err != nil {
			return err
		}
		gone := true
		for _, t := range tenants {
			if t.ID == tenantID {
				gone = false
				break
			}
		}
		if gone {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tenant %d still present after %s", tenantID, maxWait)
		}
		time.Sleep(5 * time.Second)
	}
}

// secretChars matches PBXware's allowed character set for extension secrets:
// letters, digits, and one of %*!_-
const secretUpper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
const secretLower = "abcdefghijklmnopqrstuvwxyz"
const secretDigits = "0123456789"
const secretSpecial = "%*!_-"

// GenerateSecret produces a random SIP secret satisfying PBXware's complexity
// rule: 8+ chars, at least one uppercase, one lowercase, one digit, and one
// of %*!_-  (undocumented in the API PDF; discovered empirically).
func GenerateSecret() (string, error) {
	const length = 12
	all := secretUpper + secretLower + secretDigits + secretSpecial

	// Guarantee one of each required class, then fill the rest randomly, then shuffle.
	required := []string{secretUpper, secretLower, secretDigits, secretSpecial}
	chars := make([]byte, 0, length)
	for _, class := range required {
		b, err := randChar(class)
		if err != nil {
			return "", err
		}
		chars = append(chars, b)
	}
	for len(chars) < length {
		b, err := randChar(all)
		if err != nil {
			return "", err
		}
		chars = append(chars, b)
	}
	if err := shuffleBytes(chars); err != nil {
		return "", err
	}
	return string(chars), nil
}

func randChar(class string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(class))))
	if err != nil {
		return 0, err
	}
	return class[n.Int64()], nil
}

func shuffleBytes(b []byte) error {
	for i := len(b) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return err
		}
		j := n.Int64()
		b[i], b[j] = b[j], b[i]
	}
	return nil
}
