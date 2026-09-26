package wizard

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// resetTenantGoneWait bounds how long ResetInstance waits for a tenant to
// disappear when the delete request itself failed (see ResetInstance) —
// on slow hardware a tenant delete took minutes, so this keeps headroom.
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
	clientV2 := pbxware.NewClientV2(srv.BaseURL, srv.APIKeyV2)

	if srv.TrunkID != 0 {
		progress.set(func() { progress.message = fmt.Sprintf("deleting trunk %d", srv.TrunkID) })
		if err := client.DeleteTrunk(1, srv.TrunkID); err != nil {
			warnings = append(warnings, fmt.Sprintf("deleting trunk %d: %v", srv.TrunkID, err))
		}
	}

	if isMultiTenantEdition(srv.Edition) {
		if srv.TenantID != 0 {
			progress.set(func() { progress.message = fmt.Sprintf("deleting tenant %d", srv.TenantID) })
			// v2's tenant delete is synchronous (204 once the tenant and
			// its extensions are gone, ~2.5s live), so the package delete
			// below can follow straight away. If the request itself fails
			// (e.g. a timeout) the tenant may still be going away, so wait
			// for it to disappear before touching the package.
			if err := clientV2.DeleteTenant(srv.TenantID); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting tenant %d: %v", srv.TenantID, err))
				progress.set(func() {
					progress.message = fmt.Sprintf("waiting for tenant %d to finish deleting", srv.TenantID)
				})
				if err := client.WaitForTenantGone(srv.TenantID, resetTenantGoneWait); err != nil {
					warnings = append(warnings, fmt.Sprintf("waiting for tenant %d to finish deleting: %v", srv.TenantID, err))
				}
			}
		}
		progress.set(func() { progress.message = "deleting shared tenant package" })
		if id, ok, err := clientV2.FindSwarmDialerPackage(); err != nil {
			warnings = append(warnings, fmt.Sprintf("listing packages: %v", err))
		} else if ok {
			if err := clientV2.DeletePackage(id); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting package %d: %v", id, err))
			}
		}
	} else {
		warnings = append(warnings, deleteExtensions(clientV2, srv.Extensions, progress)...)
	}

	progress.set(func() {
		progress.done = true
		progress.message = "done"
		progress.warning = strings.Join(warnings, "; ")
	})
}

// resetDeleteWorkers is how many extension deletes run at once. v2 has no
// batch delete and one delete takes ~300-500ms, so 1000 one at a time took
// over 5 minutes (confirmed live 2026-09-25). PBXware doesn't really
// support concurrent deletes: at 10 at once ~77% failed with "Unable to
// delete extension" (500); at 2-3 at once roughly 1 in 10 does, while
// throughput still scales. Kept at 2 (not 3) for headroom on slower
// hardware — reset speed matters little. Each worker retries its own
// failures (see deleteExtensionWithRetry).
const resetDeleteWorkers = 2

// deleteExtensionWithRetry retries a delete that PBXware rejected because
// another delete was in flight at the same time.
func deleteExtensionWithRetry(clientV2 *pbxware.ClientV2, id int) error {
	var err error
	for attempt := 1; attempt <= 6; attempt++ {
		if err = clientV2.DeleteExtension(pbxware.DefaultOrg, id); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	return err
}

// deleteExtensions deletes every extension in exts (non-Multi-Tenant
// editions only — on Multi-Tenant the tenant delete takes them with it),
// returning one warning per failure.
func deleteExtensions(clientV2 *pbxware.ClientV2, exts []pbxware.ProvisionedExtension, progress *ResetProgress) []string {
	var (
		mu       sync.Mutex
		warnings []string
		done     int
		wg       sync.WaitGroup
	)
	total := len(exts)
	jobs := make(chan pbxware.ProvisionedExtension)
	for w := 0; w < resetDeleteWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ext := range jobs {
				err := deleteExtensionWithRetry(clientV2, ext.ExtensionID)
				mu.Lock()
				done++
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("deleting extension %s: %v", ext.Ext, err))
				}
				n := done
				mu.Unlock()
				progress.set(func() { progress.message = fmt.Sprintf("deleting extensions (%d/%d)", n, total) })
			}
		}()
	}
	for _, ext := range exts {
		jobs <- ext
	}
	close(jobs)
	wg.Wait()
	return warnings
}
