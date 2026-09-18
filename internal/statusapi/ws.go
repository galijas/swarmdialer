package statusapi

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// Internal tool, no auth boundary to protect (see docs/gui_spec.md's
	// hosting/security decisions) — accept upgrades from any origin,
	// including a frontend served from a different dev port.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WSHandler upgrades to a WebSocket and pushes whatever getPayload()
// returns, JSON-encoded, every interval until the client disconnects.
// getPayload is a function rather than a fixed value because the
// dashboard's set of live sessions changes over time (servers get added,
// sessions get created lazily on first dial) — this handler always
// reflects current state, not whatever existed when the client connected.
func WSHandler(getPayload func() any, interval time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("statusapi: websocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if err := conn.WriteJSON(getPayload()); err != nil {
				return
			}
		}
	})
}
