// Command loadtest reads a provisioning output file (produced by
// cmd/swarmdialer) and runs a call-load test against those extensions:
// registers them all, pairs them up, places calls, holds, and hangs up.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/sipua"
	"swarmdialer/internal/statusapi"
)

func main() {
	var (
		extensionsFile = flag.String("extensions", "extensions.json", "path to a provisioning output file from cmd/swarmdialer")
		server         = flag.String("server", "", "PBXware SIP server host:port (actual network destination); defaults to the provisioning file's host, port 5060")
		domain         = flag.String("domain", "", "SIP domain to use in headers; defaults to -server")
		localIP        = flag.String("local-ip", "10.1.100.215", "our own IP, advertised in Contact headers")
		basePort       = flag.Int("base-port", 20000, "first local SIP port; extension i binds base-port+i")
		rampInterval   = flag.Duration("ramp-interval", 100*time.Millisecond, "delay between starting each successive pair's call")
		callDuration   = flag.Duration("call-duration", 30*time.Second, "how long to hold each call before hanging up")
		regTimeout     = flag.Duration("register-timeout", 15*time.Second, "per-extension registration timeout")
		dialTimeout    = flag.Duration("dial-timeout", 15*time.Second, "per-call dial timeout")
		statusAddr     = flag.String("status-addr", ":8080", "address to serve live JSON status on at /api/status while the run is in progress; empty disables it")
	)
	flag.Parse()

	data, err := os.ReadFile(*extensionsFile)
	if err != nil {
		log.Fatalf("reading %s: %v", *extensionsFile, err)
	}
	var provisioned pbxware.ProvisionResult
	if err := json.Unmarshal(data, &provisioned); err != nil {
		log.Fatalf("parsing %s: %v", *extensionsFile, err)
	}
	if len(provisioned.Extensions) < 2 {
		log.Fatalf("%s has %d extensions; need at least 2 to form a call", *extensionsFile, len(provisioned.Extensions))
	}

	dialDestination := *server
	if dialDestination == "" {
		dialDestination = fmt.Sprintf("%s:5060", trimScheme(provisioned.Host))
	}

	endpoints := make([]sipua.Endpoint, len(provisioned.Extensions))
	for i, e := range provisioned.Extensions {
		endpoints[i] = sipua.Endpoint{Username: e.Username, Password: e.Secret, AOR: e.Ext}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	state := orchestrator.NewRunState(len(endpoints) / 2)
	if *statusAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/api/status", statusapi.Handler(state))
		srv := &http.Server{Addr: *statusAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("status server on %s stopped: %v", *statusAddr, err)
			}
		}()
		defer srv.Close()
		log.Printf("live status: http://%s/api/status", *statusAddr)
	}

	start := time.Now()
	result, err := orchestrator.Run(ctx, orchestrator.Config{
		DialDestination: dialDestination,
		SIPDomain:        *domain,
		LocalIP:          *localIP,
		BaseLocalPort:    *basePort,
		RampInterval:     *rampInterval,
		CallDuration:     *callDuration,
		RegisterTimeout:  *regTimeout,
		DialTimeout:      *dialTimeout,
	}, endpoints, state)
	if err != nil {
		log.Fatalf("orchestrator run failed: %v", err)
	}
	elapsed := time.Since(start)

	var totalSent, totalRecv uint64
	for _, p := range result.Pairs {
		totalSent += p.RTPSent
		totalRecv += p.RTPRecv
		if p.Err != nil {
			log.Printf("FAILED  %s -> %s: %v", p.CallerAOR, p.CalleeAOR, p.Err)
		}
	}

	fmt.Printf("\n=== Load test summary ===\n")
	fmt.Printf("Pairs attempted: %d\n", len(result.Pairs))
	fmt.Printf("Answered:        %d\n", result.Answered())
	fmt.Printf("Failed:          %d\n", result.Failed())
	fmt.Printf("RTP packets:     sent=%d recv=%d\n", totalSent, totalRecv)
	fmt.Printf("Elapsed:         %s\n", elapsed)
}

func trimScheme(host string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(host) > len(prefix) && host[:len(prefix)] == prefix {
			return host[len(prefix):]
		}
	}
	return host
}
