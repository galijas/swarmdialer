package statusapi

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"swarmdialer/internal/orchestrator"
)

var upgrader = websocket.Upgrader{
	// Internal tool, no auth boundary to protect (see docs/gui_spec.md's
	// hosting/security decisions) — accept upgrades from any origin,
	// including a frontend served from a different dev port.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// SessionWSHandler upgrades to a WebSocket and pushes the current
// session's Snapshot every interval until the client disconnects.
// getSession is a function rather than a fixed *Session because the
// dashboard's session doesn't exist until the wizard finishes, and this
// handler needs to keep working correctly regardless of when the client
// connects relative to that.
func SessionWSHandler(getSession func() *orchestrator.Session, interval time.Duration) http.Handler {
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
			sess := getSession()
			if sess == nil {
				continue
			}
			if err := conn.WriteJSON(sess.Snapshot()); err != nil {
				return
			}
		}
	})
}
