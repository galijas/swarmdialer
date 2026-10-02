package wizard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/serverware"
	"swarmdialer/internal/store"
)

// RestartTarget is one PBXware instance whose VPS is to be restarted.
type RestartTarget struct {
	Server *store.Server
	VPS    store.VPSRef
}

// How long a VPS restart, and PBXware coming back up after it, may take.
const (
	vpsRestartWait = 5 * time.Minute
	pbxwareUpWait  = 5 * time.Minute
)

// RestartPBXware restarts the targets' VPSs one at a time (so one
// PBXware instance is always up while the other restarts), and after
// each waits until PBXware answers again: its API, and Asterisk answering
// SIP. Used to apply a raised recording RAM disk (see ConnectServerware).
// Marks progress done; the caller doesn't need to.
func RestartPBXware(sw *serverware.Client, targets []RestartTarget, progress *ServerwareProgress) error {
	fail := func(err error) error {
		progress.set(func() { progress.done = true; progress.err = err.Error(); progress.message = "" })
		return err
	}
	for _, t := range targets {
		name, vps := t.Server.Name, t.VPS.Name
		progress.step(fmt.Sprintf("restarting %s's VPS (%s)", name, vps))
		if err := sw.RestartVPS(t.VPS.ID, vpsRestartWait); err != nil {
			return fail(fmt.Errorf("restarting %s's VPS (%s): %w", name, vps, err))
		}
		progress.completed(fmt.Sprintf("%s restarted", vps))
		progress.step(fmt.Sprintf("waiting for PBXware on %s to start (API and Asterisk)", name))
		if err := waitPBXwareUp(t.Server, pbxwareUpWait); err != nil {
			return fail(fmt.Errorf("%s's VPS restarted, but %w", name, err))
		}
		progress.completed(fmt.Sprintf("PBXware on %s is up (API answers, Asterisk answers SIP)", name))
	}
	progress.Finish()
	return nil
}

// waitPBXwareUp waits until the instance's API answers and Asterisk
// answers a SIP OPTIONS request: the API alone can be up before Asterisk
// has started.
func waitPBXwareUp(srv *store.Server, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	api := pbxware.NewClient(srv.BaseURL, srv.APIKey)
	var apiErr, sipErr error
	for {
		if _, apiErr = api.GetLicenseInfo(); apiErr == nil {
			if sipErr = sipOptionsPing(srv.SIPHost, 2*time.Second); sipErr == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if apiErr != nil {
				return fmt.Errorf("its API still doesn't answer after %s: %v", wait, apiErr)
			}
			return fmt.Errorf("Asterisk still doesn't answer SIP after %s: %v", wait, sipErr)
		}
		time.Sleep(3 * time.Second)
	}
}

// sipOptionsPing sends one SIP OPTIONS request over UDP to hostPort and
// waits for any SIP response. Asterisk answers OPTIONS (200, or 401/404
// depending on its settings) whenever it is running, so any response
// means it's up.
func sipOptionsPing(hostPort string, timeout time.Duration) error {
	conn, err := net.DialTimeout("udp", hostPort, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	host, _, _ := net.SplitHostPort(hostPort)
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	req := strings.Join([]string{
		fmt.Sprintf("OPTIONS sip:%s SIP/2.0", host),
		fmt.Sprintf("Via: SIP/2.0/UDP %s;branch=z9hG4bK%s;rport", local, id),
		"Max-Forwards: 70",
		fmt.Sprintf("From: <sip:swarmdialer@%s>;tag=%s", local.IP, id[:8]),
		fmt.Sprintf("To: <sip:%s>", host),
		fmt.Sprintf("Call-ID: %s@swarmdialer", id),
		"CSeq: 1 OPTIONS",
		fmt.Sprintf("Contact: <sip:swarmdialer@%s>", local),
		"User-Agent: SwarmDialer",
		"Content-Length: 0",
		"", "",
	}, "\r\n")
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("no answer to SIP OPTIONS: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "SIP/2.0 ") {
		return fmt.Errorf("unexpected answer to SIP OPTIONS")
	}
	return nil
}
