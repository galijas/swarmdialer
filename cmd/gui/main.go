// Command gui is SwarmDialer's actual product: a web server exposing the
// Setup Wizard + Dashboard described in docs/gui_spec.md. This is what
// gets deployed on a fresh VPS — see that doc's portability requirement:
// no hardcoded IPs/keys, everything comes from the wizard at runtime.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
)

func main() {
	// Port 80 so the wizard/dashboard is reachable at just http://<host>/ —
	// no port to remember or type. Binding it requires root (or
	// CAP_NET_BIND_SERVICE) on Linux; every deployment path we ship
	// (install.sh, manual `sudo ./bin/gui`) already runs as root.
	addr := flag.String("addr", ":80", "address to serve the GUI on")
	configPath := flag.String("config", "swarmdialer_config.json", "path to the persisted config file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	app, err := newApp(ctx, *configPath)
	if err != nil {
		log.Fatalf("starting: %v", err)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	log.Printf("SwarmDialer GUI listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
