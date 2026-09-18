// Command swarmdialer currently implements the provisioning step: creating
// (or reusing) a PBXware tenant and adding N test extensions to it, ready for
// the SIP/RTP call-generation stage to dial between.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"swarmdialer/internal/pbxware"
)

func main() {
	var (
		baseURL       = flag.String("base-url", os.Getenv("SWARMDIALER_PBXWARE_URL"), "PBXware base URL, e.g. https://10.1.100.208 (env: SWARMDIALER_PBXWARE_URL)")
		apiKey        = flag.String("api-key", os.Getenv("SWARMDIALER_PBXWARE_APIKEY"), "PBXware API key (env: SWARMDIALER_PBXWARE_APIKEY)")
		tenantID      = flag.Int("tenant-id", 0, "existing tenant/server ID to add extensions to; 0 means create a new tenant")
		tenantCode    = flag.String("tenant-code", "", "3-digit tenant code to use when creating a new tenant (required if -tenant-id=0)")
		tenantName    = flag.String("tenant-name", "swarmdialer.local", "tenant name to use when creating a new tenant")
		extLength     = flag.Int("ext-length", 3, "extension number length for a newly created tenant")
		pkg           = flag.String("package", "1", "tenant package ID to use when creating a new tenant")
		country       = flag.String("country", "869", "route ID for tenant country (869 = USA)")
		national      = flag.String("national", "1", "national dialing code for a newly created tenant")
		international = flag.String("international", "011", "international dialing code for a newly created tenant")
		count         = flag.Int("count", 2, "number of extensions to create")
		extStart      = flag.Int("ext-start", 100, "first extension number to assign; subsequent extensions increment from here")
		ua            = flag.Int("ua", 50, "User Agent Device ID (50 = Generic SIP)")
		maxWait       = flag.Duration("max-wait", 10*time.Minute, "how long to keep retrying extension creation while a tenant finishes provisioning")
		output        = flag.String("output", "extensions.json", "path to write the created extensions (with SIP credentials) as JSON")
		channelLimit  = flag.Int("channel-limit", 600, "local/remote channel limit to set on the tenant (PBXware defaults every tenant to 8, far too low for load testing) — applied whether the tenant is newly created or reused")
	)
	flag.Parse()

	if *baseURL == "" || *apiKey == "" {
		log.Fatal("-base-url and -api-key (or their env vars) are required")
	}

	client := pbxware.NewClient(*baseURL, *apiKey)

	server := *tenantID
	if server == 0 {
		if *tenantCode == "" {
			log.Fatal("-tenant-code is required when creating a new tenant (-tenant-id=0)")
		}
		id, err := client.AddTenant(pbxware.TenantParams{
			Name:          *tenantName,
			Code:          *tenantCode,
			Package:       *pkg,
			ExtLength:     *extLength,
			Country:       *country,
			National:      *national,
			International: *international,
		})
		if err != nil {
			log.Printf("tenant.add returned an error (%v) — checking tenant.list before giving up, since PBXware sometimes creates the tenant despite a timed-out/erroring response", err)
			id, err = findTenantByCode(client, *tenantCode)
			if err != nil {
				log.Fatalf("could not create or find tenant with code %s: %v", *tenantCode, err)
			}
		}
		log.Printf("using tenant ID %d (code %s)", id, *tenantCode)
		server = id
	}

	if err := client.WaitForTenant(server, *maxWait); err != nil {
		log.Fatalf("tenant %d never showed up: %v", server, err)
	}

	// PBXware defaults every tenant's channel-like resource pools (Local/
	// Remote SIP channels, Conferences, Queues, Enhanced Ring Groups, Auto
	// Attendants, DAHDI) to 8 each, independently, regardless of
	// package/license — always raise them all, whether this tenant was
	// just created or is being reused, since existing/GUI-created tenants
	// default to 8 too. See SetTenantChannelLimits's doc comment.
	if err := client.SetTenantChannelLimits(server, *channelLimit, *maxWait); err != nil {
		log.Fatalf("raising tenant %d's channel limit: %v", server, err)
	}
	log.Printf("tenant %d channel limit set to %d", server, *channelLimit)

	var created []pbxware.ProvisionedExtension
	for i := 0; i < *count; i++ {
		extNum := *extStart + i
		secret, err := pbxware.GenerateSecret()
		if err != nil {
			log.Fatalf("generating secret: %v", err)
		}

		result, err := client.AddExtensionWithRetry(pbxware.ExtensionParams{
			Server:        server,
			Name:          fmt.Sprintf("SwarmDialer %d", extNum),
			Email:         fmt.Sprintf("swarmdialer%d@swarmdialer.local", extNum),
			Ext:           fmt.Sprint(extNum),
			Secret:        secret,
			UA:            *ua,
			IncomingLimit: 2,
			OutgoingLimit: 2,
			AllowedCodecs: "ulaw:alaw",
		}, *maxWait)
		if err != nil {
			log.Fatalf("creating extension %d: %v", extNum, err)
		}

		cfg, err := client.GetExtensionConfig(server, result.ExtensionID)
		if err != nil {
			log.Fatalf("fetching config for extension %d: %v", extNum, err)
		}

		created = append(created, pbxware.ProvisionedExtension{
			ExtensionID: result.ExtensionID,
			Ext:         cfg.Ext,
			Username:    cfg.Username,
			Secret:      cfg.Secret,
		})
		log.Printf("created extension %s (id %d, username %s)", cfg.Ext, result.ExtensionID, cfg.Username)
	}

	f, err := os.Create(*output)
	if err != nil {
		log.Fatalf("writing output file: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(pbxware.ProvisionResult{
		Server:     server,
		Host:       *baseURL,
		Extensions: created,
	}); err != nil {
		log.Fatalf("encoding output file: %v", err)
	}

	log.Printf("done: %d extensions written to %s", len(created), *output)
}

func findTenantByCode(client *pbxware.Client, code string) (int, error) {
	tenants, err := client.ListTenants()
	if err != nil {
		return 0, err
	}
	for _, t := range tenants {
		if t.Code == code {
			return t.ID, nil
		}
	}
	return 0, fmt.Errorf("no tenant with code %s found", code)
}
