package pbxware

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

// AddExtensionWithRetry calls AddExtension, retrying with exponential backoff
// until it succeeds or maxWait elapses.
//
// It deliberately retries on ANY error rather than trying to whitelist known
// "tenant not ready yet" error strings: empirically, a freshly created
// tenant's not-ready state has produced at least four different error shapes
// from PBXware ("Failed to generate configuration", "Table ... doesn't
// exist", a raw HTML error page instead of JSON, "Unable to create
// extension") and there's no reason to assume that list is complete. maxWait
// bounds the cost of this: a genuinely permanent error (bad input, etc.)
// still surfaces correctly once the deadline is hit, just slower than an
// immediate failure would.
func (c *Client) AddExtensionWithRetry(p ExtensionParams, maxWait time.Duration) (ExtensionResult, error) {
	delay := 5 * time.Second
	const maxDelay = 60 * time.Second
	deadline := time.Now().Add(maxWait)

	for {
		result, err := c.AddExtension(p)
		if err == nil {
			return result, nil
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
