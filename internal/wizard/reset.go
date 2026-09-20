package wizard

import (
	"fmt"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// resetTenantGoneWait bounds how long ResetInstance waits for a deleted
// tenant to actually disappear before touching the shared package —
// tenant deletion is as slow as tenant creation on this system (minutes,
// not seconds), so this needs real headroom, not a token pause.
const resetTenantGoneWait = 5 * time.Minute

// ResetInstance deletes everything SwarmDialer created on srv's PBXware
// instance: for Multi-Tenant editions, the trunk, the tenant, and the
// shared SwarmDialerPackage (tenant deletion takes the tenant's own
// extensions with it); for other editions (no tenant concept), the trunk
// and every extension SwarmDialer provisioned individually.
//
// Best-effort: a failure on one step (e.g. the trunk was already removed
// manually) doesn't stop the rest, since the caller should end up with as
// clean a slate as possible either way and srv gets removed from the
// store regardless. Every failure is returned as a warning string rather
// than an error — a nil/empty return means everything succeeded.
func ResetInstance(srv *store.Server) []string {
	var warnings []string
	client := pbxware.NewClient(srv.BaseURL, srv.APIKey)

	if srv.TrunkID != 0 {
		if err := client.DeleteTrunk(1, srv.TrunkID); err != nil {
			warnings = append(warnings, fmt.Sprintf("deleting trunk %d: %v", srv.TrunkID, err))
		}
	}

	if isMultiTenantEdition(srv.Edition) {
		if srv.TenantID != 0 {
			if err := client.DeleteTenant(srv.TenantID); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting tenant %d: %v", srv.TenantID, err))
			} else if err := client.WaitForTenantGone(srv.TenantID, resetTenantGoneWait); err != nil {
				// Tenant delete was accepted but hasn't finished — still try
				// the package below (best-effort), but flag it since
				// deleting the package out from under a still-deleting
				// tenant could otherwise fail silently or leave things
				// half-cleaned.
				warnings = append(warnings, fmt.Sprintf("waiting for tenant %d to finish deleting: %v", srv.TenantID, err))
			}
		}
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
		for _, ext := range srv.Extensions {
			if err := client.DeleteExtension(1, ext.ExtensionID); err != nil {
				warnings = append(warnings, fmt.Sprintf("deleting extension %s: %v", ext.Ext, err))
			}
		}
	}

	return warnings
}
