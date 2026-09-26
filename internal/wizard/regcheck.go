package wizard

import (
	"context"
	"fmt"
	"net"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/sipua"
)

// registrationReadyWait bounds how long waitUntilRegistrable keeps trying.
// The window observed live was ~24s after creating 1000 extensions.
const registrationReadyWait = 3 * time.Minute

// waitUntilRegistrable blocks until PBXware accepts a real SIP REGISTER for
// both the first and the last of exts. Confirmed live 2026-09-25: right
// after a batch of extensions is created (on both editions), PBXware
// rejects every registration with 403 for a while — ~24s after 1000
// extensions, including for extensions created a minute earlier — so it
// reloads the whole endpoint set once after the last change rather than per
// extension. Without this wait, the first dial right after the wizard
// finishes fails for reasons that have nothing to do with load.
//
// Each probe registers from a throwaway port and then unregisters
// (Expires: 0), so it leaves no binding behind to contend with a later
// dialer session's own registration.
func waitUntilRegistrable(ctx context.Context, localIP, sipHost string, exts []pbxware.ProvisionedExtension) error {
	if len(exts) == 0 {
		return nil
	}
	probes := []pbxware.ProvisionedExtension{exts[0]}
	if len(exts) > 1 {
		probes = append(probes, exts[len(exts)-1])
	}

	deadline := time.Now().Add(registrationReadyWait)
	for _, ext := range probes {
		for {
			err := probeRegister(ctx, localIP, sipHost, ext)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("extension %s still can't register after %s: %w", ext.Ext, registrationReadyWait, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	return nil
}

// probeRegister registers ext once from a free local port, then removes
// the binding again.
func probeRegister(ctx context.Context, localIP, sipHost string, ext pbxware.ProvisionedExtension) error {
	port, err := freeUDPPort()
	if err != nil {
		return err
	}
	phoneCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	phone, err := sipua.NewPhone(phoneCtx, localIP, port, sipua.Endpoint{
		Username: ext.Username, Password: ext.Secret, AOR: ext.Ext,
	})
	if err != nil {
		return fmt.Errorf("setting up probe phone: %w", err)
	}
	defer phone.Close()

	regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
	defer regCancel()
	if _, err := phone.Register(regCtx, sipHost, sipHost, 60); err != nil {
		return err
	}
	_, _ = phone.Register(regCtx, sipHost, sipHost, 0) // best-effort unregister
	return nil
}

// freeUDPPort asks the OS for a currently unused UDP port.
func freeUDPPort() (int, error) {
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return 0, fmt.Errorf("finding a free UDP port: %w", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port, nil
}
