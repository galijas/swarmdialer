package wizard

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// resetTenantGoneWait bounds how long ResetInstance waits for a deleted
// tenant to actually disappear before touching the shared package —
// tenant deletion is as slow as tenant creation on this system (minutes,
// not seconds), so this needs real headroom, not a token pause.
const resetTenantGoneWait = 5 * time.Minute

// ResetProgress reports live progress of ResetInstance — same
// mutex-guarded snapshot pattern as ProvisionProgress/ConnectProgress,
// since resetting a Multi-Tenant instance's tenant is just as slow as
// creating one and the caller needs something to show while it waits.
type ResetProgress struct {
	mu      sync.Mutex
	message string
	done    bool
	warning string // joined warnings, if any — still "done", not a hard error
}

func (p *ResetProgress) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// ResetProgressSnapshot is a point-in-time, serializable copy of a
// ResetProgress — kept separate since ResetProgress embeds a sync.Mutex
// (see ProvisionProgressSnapshot's doc comment in wizard.go for why).
type ResetProgressSnapshot struct {
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Warning string `json:"warning,omitempty"`
}

// Snapshot returns a copy safe to read or serialize without locking.
func (p *ResetProgress) Snapshot() ResetProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ResetProgressSnapshot{Message: p.message, Done: p.done, Warning: p.warning}
}

// AppendWarning adds one more warning to an already-finished progress —
// used by the caller for a failure that happens after ResetInstance
// returns (e.g. removing srv from the store), so it still surfaces to
// whoever's polling instead of only being logged server-side.
func (p *ResetProgress) AppendWarning(w string) {
	p.set(func() {
		if p.warning != "" {
			p.warning += "; "
		}
		p.warning += w
	})
}

// ResetInstance deletes everything SwarmDialer created on srv's PBXware
// instance: for Multi-Tenant editions, the trunk, the tenant, and the
// shared SwarmDialerPackage (tenant deletion takes the tenant's own
// extensions with it); for other editions (no tenant concept), the trunk
// and every extension SwarmDialer provisioned individually. Pass a fresh
// &ResetProgress{} and poll Snapshot from elsewhere while this runs — it
// blocks until done (there's no failure exit; see below).
//
// Best-effort: a failure on one step (e.g. the trunk was already removed
// manually) doesn't stop the rest, since the caller should end up with as
// clean a slate as possible either way and srv gets removed from the
// store regardless. Every failure is recorded as a warning on progress
// rather than returned as an error — Snapshot().Warning is empty if
// everything succeeded.
func ResetInstance(srv *store.Server, progress *ResetProgress) {
	var warnings []string
	client := pbxware.NewClient(srv.BaseURL, srv.APIKey)

	if srv.TrunkID != 0 {
		progress.set(func() { progress.message = fmt.Sprintf("deleting trunk %d", srv.TrunkID) })
		if err := client.DeleteTrunk(1, srv.TrunkID); err != nil {
			warnings = append(warnings, fmt.Sprintf("deleting trunk %d: %v", srv.TrunkID, err))
		}
	}

	if isMultiTenantEdition(srv.Edition) {
		if srv.TenantID != 0 {
			progress.set(func() { progress.message = fmt.Sprintf("deleting tenant %d", srv.TenantID) })
			// tenant.delete frequently outlasts the 30s HTTP client timeout
			// even when it succeeds server-side — the same behavior already
			// documented on AddTenant for tenant.add. A timeout/transport
			// error here does NOT mean the tenant is still there, so it's
			// recorded as a warning but doesn't skip the wait-for-gone
			// check below — that check is the only way to find out what
			// actually happened, and skipping it (as an earlier version of
			// this function did) meant the package delete right after would
			// run while the tenant might still be present, failing with
			// "Cannot delete package that is currently assigned to
			// tenants" instead of the real problem being visible.
			if err := client.DeleteTenant(srv.TenantID); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting tenant %d: %v", srv.TenantID, err))
			}
			progress.set(func() {
				progress.message = fmt.Sprintf("waiting for tenant %d to finish deleting (can take several minutes)", srv.TenantID)
			})
			if err := client.WaitForTenantGone(srv.TenantID, resetTenantGoneWait); err != nil {
				warnings = append(warnings, fmt.Sprintf("waiting for tenant %d to finish deleting: %v", srv.TenantID, err))
			}
		}
		progress.set(func() { progress.message = "deleting shared tenant package" })
		packages, err := client.ListPackages()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("listing packages: %v", err))
		} else {
			for id, name := range packages {
				if name == pbxware.SwarmDialerPackageName {
					if err := client.DeletePackage(id); err != nil {
						warnings = append(warnings, fmt.Sprintf("deleting package %d: %v", id, err))
					}
					break
				}
			}
		}
	} else {
		total := len(srv.Extensions)
		for i, ext := range srv.Extensions {
			num, extNum := i+1, ext.Ext
			progress.set(func() {
				progress.message = fmt.Sprintf("deleting extension %s (%d/%d)", extNum, num, total)
			})
			if err := client.DeleteExtension(1, ext.ExtensionID); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting extension %s: %v", extNum, err))
			}
		}
	}

	progress.set(func() {
		progress.done = true
		progress.message = "done"
		progress.warning = strings.Join(warnings, "; ")
	})
}
