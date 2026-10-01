// Command gui is SwarmDialer's actual product: a web server exposing the
// Setup Wizard + Dashboard described in docs/gui_spec.md. This is what
// gets deployed on a fresh VPS — see that doc's portability requirement:
// no hardcoded IPs/keys, everything comes from the wizard at runtime.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"swarmdialer/internal/sipua"
)

func main() {
	// HTTPS on 443, plus a plain-HTTP listener on 80 that only redirects to
	// it, so the GUI is still reachable at just https://<host>/. Binding
	// both requires root (or CAP_NET_BIND_SERVICE) on Linux; every
	// deployment path we ship (install.sh) runs as root.
	addr := flag.String("addr", ":443", "HTTPS address to serve the GUI on")
	redirectAddr := flag.String("http-redirect-addr", ":80", "plain-HTTP address that redirects to HTTPS (empty to disable)")
	configPath := flag.String("config", "swarmdialer_config.json", "path to the persisted config file")
	tlsDir := flag.String("tls-dir", "tls", "directory holding the GUI's TLS certificate and key (a self-signed pair is created if missing)")
	reset := flag.Bool("reset-password", false, "create a new random admin password, print it, and exit")
	flag.Parse()

	authPath := authPathFor(*configPath)
	if *reset {
		pw, err := resetPassword(authPath)
		if err != nil {
			log.Fatalf("resetting password: %v", err)
		}
		fmt.Printf("SwarmDialer admin login\n  Username: %s\n  Password: %s\n", authUsername, pw)
		fmt.Println("(If SwarmDialer is running, restart it to apply: systemctl restart swarmdialer)")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Encode the calls' audio now (about a second of CPU), not during the
	// first call that needs it.
	go sipua.PrepareAudio()

	auth, err := newAuthManager(authPath)
	if err != nil {
		log.Fatalf("loading admin login: %v", err)
	}
	app, err := newApp(ctx, *configPath)
	if err != nil {
		log.Fatalf("starting: %v", err)
	}
	// The config holds admin API keys: root-only, including for config
	// files written before this was enforced.
	_ = os.Chmod(*configPath, 0o600)

	certPath, keyPath, fingerprint, err := ensureSelfSignedCert(*tlsDir)
	if err != nil {
		log.Fatalf("preparing TLS certificate: %v", err)
	}
	log.Printf("TLS certificate SHA-256 fingerprint: %s", fingerprint)

	mux := http.NewServeMux()
	app.registerRoutes(mux)
	mux.HandleFunc("/api/login", auth.handleLogin)
	mux.HandleFunc("/api/logout", auth.handleLogout)
	mux.HandleFunc("/api/account/password", auth.handleChangePassword)

	srv := &http.Server{Addr: *addr, Handler: auth.require(mux)}
	var redirect *http.Server
	if *redirectAddr != "" {
		redirect = &http.Server{Addr: *redirectAddr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
				host = host[:i]
			}
			if p := strings.TrimPrefix(*addr, ":"); p != "443" && p != "" && !strings.Contains(*addr, "]") && !strings.Contains(p, ":") {
				host += ":" + p
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
		})}
		go func() {
			if err := redirect.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("HTTP redirect listener on %s stopped: %v", *redirectAddr, err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		srv.Close()
		if redirect != nil {
			redirect.Close()
		}
	}()

	log.Printf("SwarmDialer GUI listening on https://%s (HTTP %s redirects to it)", *addr, *redirectAddr)
	if err := srv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
