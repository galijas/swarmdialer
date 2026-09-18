// Command sip-test is a scratch tool for verifying the sipua package against
// the real PBXware instance one milestone at a time (register, then dial,
// then RTP). It will be folded into the main swarmdialer CLI once the SIP/RTP
// pieces are all proven out — see PROJECT_STATE.md.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/emiago/sipgo"

	"swarmdialer/internal/sipua"
)

func main() {
	var (
		server    = flag.String("server", "10.1.100.208:5060", "PBXware SIP server host:port (actual network destination)")
		domain    = flag.String("domain", "", "SIP domain to use in headers (Request-URI/From/To); defaults to -server if unset. Try the tenant's FQDN (tenant_name) here if -server fails")
		localHost = flag.String("local-host", "10.1.100.215", "our own IP, advertised in the Contact header")
		username  = flag.String("username", "", "SIP auth username, e.g. 999200 (tenant_code+ext)")
		password  = flag.String("password", "", "SIP secret")
		aor       = flag.String("aor", "", "extension number to register as, e.g. 200")
	)
	flag.Parse()

	if *username == "" || *password == "" || *aor == "" {
		log.Fatal("-username, -password, and -aor are required")
	}
	sipDomain := *domain
	if sipDomain == "" {
		sipDomain = *server
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		log.Fatalf("creating user agent: %v", err)
	}
	defer ua.Close()

	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname(*localHost))
	if err != nil {
		log.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	granted, err := sipua.Register(ctx, client, *server, sipDomain, *localHost, sipua.Endpoint{
		Username: *username,
		Password: *password,
		AOR:      *aor,
	}, 300)
	if err != nil {
		log.Fatalf("registration failed: %v", err)
	}

	log.Printf("registered extension %s successfully, granted expiry %ds", *aor, granted)
}
