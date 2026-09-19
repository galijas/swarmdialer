// Command call-test is a scratch tool for verifying end-to-end call setup
// (REGISTER both sides, INVITE, answer, ACK, then BYE) between two PBXware
// extensions. Like cmd/sip-test, this will be folded into the main
// swarmdialer binary once the SIP/RTP pieces are all proven out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"swarmdialer/internal/sipua"
)

func main() {
	var (
		server       = flag.String("server", "10.1.100.208:5060", "PBXware SIP server host:port (actual network destination)")
		domain       = flag.String("domain", "", "SIP domain to use in headers; defaults to -server if unset")
		localIP      = flag.String("local-ip", "10.1.100.215", "our own IP, advertised in Contact headers")
		callerPort   = flag.Int("caller-port", 15200, "local UDP port for the calling phone")
		calleePort   = flag.Int("callee-port", 15201, "local UDP port for the answering phone")
		callerUser   = flag.String("caller-username", "", "caller's SIP auth username, e.g. 999200")
		callerPass   = flag.String("caller-password", "", "caller's SIP secret")
		callerAOR    = flag.String("caller-aor", "", "caller's extension number, e.g. 200")
		calleeUser   = flag.String("callee-username", "", "callee's SIP auth username, e.g. 999201")
		calleePass   = flag.String("callee-password", "", "callee's SIP secret")
		calleeAOR    = flag.String("callee-aor", "", "callee's extension number, e.g. 201")
		callDuration = flag.Duration("call-duration", 5*time.Second, "how long to hold the call before hanging up")
	)
	flag.Parse()

	if *callerUser == "" || *callerPass == "" || *callerAOR == "" || *calleeUser == "" || *calleePass == "" || *calleeAOR == "" {
		log.Fatal("-caller-username/-caller-password/-caller-aor and -callee-username/-callee-password/-callee-aor are all required")
	}
	sipDomain := *domain
	if sipDomain == "" {
		sipDomain = *server
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caller, err := sipua.NewPhone(ctx, *localIP, *callerPort, sipua.Endpoint{
		Username: *callerUser, Password: *callerPass, AOR: *callerAOR,
	})
	if err != nil {
		log.Fatalf("setting up caller phone: %v", err)
	}
	defer caller.Close()

	callee, err := sipua.NewPhone(ctx, *localIP, *calleePort, sipua.Endpoint{
		Username: *calleeUser, Password: *calleePass, AOR: *calleeAOR,
	})
	if err != nil {
		log.Fatalf("setting up callee phone: %v", err)
	}
	defer callee.Close()

	regCtx, regCancel := context.WithTimeout(ctx, 15*time.Second)
	defer regCancel()
	if _, err := caller.Register(regCtx, *server, sipDomain, 300); err != nil {
		log.Fatalf("registering caller: %v", err)
	}
	log.Printf("caller %s registered", *callerAOR)

	if _, err := callee.Register(regCtx, *server, sipDomain, 300); err != nil {
		log.Fatalf("registering callee: %v", err)
	}
	log.Printf("callee %s registered", *calleeAOR)

	callee.AutoAnswer(ctx)

	call, err := caller.Dial(ctx, 15*time.Second, *server, sipDomain, *calleeAOR, true)
	if err != nil {
		log.Fatalf("dialing %s: %v", *calleeAOR, err)
	}
	log.Printf("call answered: %s -> %s", *callerAOR, *calleeAOR)

	fmt.Printf("call established, holding for %s...\n", *callDuration)
	time.Sleep(*callDuration)

	sent, recv := call.RTP.Stats()
	log.Printf("RTP stats before hangup: sent=%d recv=%d packets", sent, recv)

	byeCtx, byeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer byeCancel()
	if err := call.Hangup(byeCtx); err != nil {
		log.Fatalf("sending bye: %v", err)
	}
	log.Println("call ended cleanly")
}
