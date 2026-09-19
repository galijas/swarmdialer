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
	addr := flag.String("addr", ":8080", "address to serve the GUI on")
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
