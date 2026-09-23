package wizard

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"swarmdialer/internal/pbxware"
)

// minChannelsForCodecSelector is the system-wide Local/Remote channel
// limit SwarmDialer expects for real concurrent-call testing — PBXware
// defaults to 246 regardless of license, which caps real concurrency well
// below what a load test needs (see docs/PROJECT_STATE.md's 2026-09-21
// finding). 512 matches the license default and what the wizard's own
// warning message tells the user to set it to.
const minChannelsForCodecSelector = 512

// SystemSettingsCheck is what VerifySystemSettings reports about a
// non-Multi-Tenant instance's system-wide settings — both of which are
// GUI-only (no API write path exists for either, confirmed live
// 2026-09-21/23 — see the channel-limit and codec-allowlist findings in
// docs/PROJECT_STATE.md), so the wizard can only check whether the user
// already made these changes manually, not make them itself.
type SystemSettingsCheck struct {
	ChannelsOK      bool
	CurrentChannels int
	CodecsOK        bool
}

// VerifySystemSettings checks whether a non-Multi-Tenant PBXware
// instance's system-wide Local/Remote channel limit and codec allowlist
// have been manually raised — see the Setup Wizard's blocking prompt for
// non-Multi-Tenant instances (cmd/gui/web: the "raise channels and
// codecs manually" step), which calls this to decide whether to proceed,
// re-prompt, or let the user skip codec-selector support for this
// instance.
//
// Channels are read directly from pbxware.server.configuration. Codecs
// have no equivalent read — server.configuration never includes them,
// even once set via the admin GUI (confirmed live) — so this verifies
// them indirectly: create a throwaway extension, try to set its allowed
// codecs to SwarmDialer's full list, read back what actually stuck (a
// per-extension acodecs value can only include what the *system* already
// allows — confirmed live 2026-09-23, the same mechanism already relied
// on for Multi-Tenant tenants), and delete it again. If g729 and opus
// both survive, the system-level allowlist includes them.
func VerifySystemSettings(baseURL, apiKey string) (SystemSettingsCheck, error) {
	client := pbxware.NewClient(baseURL, apiKey)

	config, err := client.GetSystemConfiguration()
	if err != nil {
		return SystemSettingsCheck{}, fmt.Errorf("reading server configuration: %w", err)
	}
	check := SystemSettingsCheck{
		CurrentChannels: config.IncomingLimit,
		ChannelsOK:      config.IncomingLimit >= minChannelsForCodecSelector && config.OutgoingLimit >= minChannelsForCodecSelector,
	}

	// GetSystemExtensionLength reads PBXware's "numbering" field, which is
	// only present in pbxware.server.configuration's response before any
	// extensions exist yet (confirmed live 2026-09-23: it's absent on an
	// already-provisioned instance) — exactly the state a fresh instance
	// is in at this point in the wizard, so this should normally succeed.
	// If it doesn't (e.g. this instance already has extensions from
	// before), 4 digits is a safe default — every non-Multi-Tenant
	// instance this project has used has been 4-digit, and getting this
	// wrong just means the throwaway test extension below fails to
	// create, which still degrades gracefully to CodecsOK=false rather
	// than a hard error.
	digits, err := client.GetSystemExtensionLength()
	if err != nil {
		digits = 4
	}
	// A single random guess can collide with a real extension the target
	// instance already has (confirmed live 2026-09-23, against an
	// instance with 500+ existing extensions) — a few attempts with a
	// fresh random number makes this robust against that bad luck without
	// needing the full retry-with-backoff machinery wizard.Provision uses
	// for bulk creation (this is a single one-off check, not a batch).
	const maxAttempts = 5
	var result pbxware.ExtensionResult
	created := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		testExt := randomTestExtension(digits)
		result, err = client.AddExtension(pbxware.ExtensionParams{
			Server: 1, Name: "SwarmDialer codec test", Email: "swarmdialer-codec-test@swarmdialer.local",
			Ext: testExt, Secret: "Tmp1234!Check", UA: 50, IncomingLimit: 1, OutgoingLimit: 1,
			AllowedCodecs: pbxware.SwarmDialerCodecs,
		})
		if err == nil {
			created = true
			break
		}
	}
	if !created {
		return check, nil // couldn't create a test extension after retries — leave CodecsOK false rather than fail the whole check
	}
	defer func() { _ = client.DeleteExtension(1, result.ExtensionID) }()

	cfg, err := client.GetExtensionConfig(1, result.ExtensionID)
	if err != nil {
		return check, nil
	}
	check.CodecsOK = strings.Contains(cfg.AllowedCodecsRaw, "g729") && strings.Contains(cfg.AllowedCodecsRaw, "opus")
	return check, nil
}

func randomTestExtension(digits int) string {
	base := 1
	for i := 1; i < digits; i++ {
		base *= 10
	}
	// Bias toward the high end of the digit range — less likely to
	// collide with real (low-numbered) extensions a user might already
	// have provisioned.
	n := base + int(rand.Int32N(int32(base*9/10))) + base*9/10
	return strconv.Itoa(n)
}
