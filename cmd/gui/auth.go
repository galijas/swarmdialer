package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Login protects the whole GUI: it holds admin API keys for both PBXware
// instances (and SERVERware), so anyone who can reach it could otherwise
// control them. There is one admin account; its password is created at
// install time (see -reset-password) and stored only as a salted
// PBKDF2-SHA256 hash, in its own root-only file next to the config — kept
// separate so "Reset SwarmDialer" (which deletes the config) doesn't lock
// the admin out.

const (
	authUsername     = "admin"
	pbkdf2Iterations = 600_000 // OWASP's current recommendation for PBKDF2-SHA256
	sessionCookie    = "swarmdialer_session"
	sessionLifetime  = 12 * time.Hour

	// Failed logins from one client IP are slowed down after
	// loginSlowAfter failures and locked out for loginLockout after
	// loginLockAfter, so the password can't be brute-forced.
	loginSlowAfter = 3
	loginLockAfter = 10
	loginLockout   = 15 * time.Minute
)

type authRecord struct {
	Username   string `json:"username"`
	Salt       string `json:"salt"` // base64
	Hash       string `json:"hash"` // base64
	Iterations int    `json:"iterations"`
}

type loginFailures struct {
	count       int
	lockedUntil time.Time
}

type authManager struct {
	path string

	mu       sync.Mutex
	record   authRecord
	sessions map[string]time.Time // session token -> expiry
	failures map[string]*loginFailures
}

// authPathFor returns the password file path for a config path.
func authPathFor(configPath string) string {
	dir := "."
	if i := strings.LastIndex(configPath, "/"); i >= 0 {
		dir = configPath[:i]
	}
	return dir + "/swarmdialer_auth.json"
}

// newAuthManager loads the password file. If there is none (a deployment
// that predates login, or a deleted file), it creates a random password
// rather than ever serving the GUI unprotected, and prints it to the log
// once so the admin can retrieve it (journalctl -u swarmdialer).
func newAuthManager(path string) (*authManager, error) {
	m := &authManager{path: path, sessions: map[string]time.Time{}, failures: map[string]*loginFailures{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		pw, err := resetPassword(path)
		if err != nil {
			return nil, err
		}
		log.Printf("no admin password was set, so one was created. Username: %s  Password: %s  (change it with: bin/gui -reset-password)", authUsername, pw)
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m.record); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return m, nil
}

// resetPassword generates a new random admin password, stores its hash at
// path (root-only), and returns the plaintext so it can be shown once.
func resetPassword(path string) (string, error) {
	pw := randomToken(18) // 24 URL-safe characters
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	rec := authRecord{
		Username:   authUsername,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Hash:       base64.StdEncoding.EncodeToString(hash),
		Iterations: pbkdf2Iterations,
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return pw, nil
}

func randomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // the OS RNG failing is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *authManager) checkPassword(username, password string) bool {
	salt, err1 := base64.StdEncoding.DecodeString(m.record.Salt)
	want, err2 := base64.StdEncoding.DecodeString(m.record.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, m.record.Iterations, len(want))
	if err != nil {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(m.record.Username)) == 1
	return subtle.ConstantTimeCompare(got, want) == 1 && userOK
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (m *authManager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)

	m.mu.Lock()
	f := m.failures[ip]
	if f != nil && time.Now().Before(f.lockedUntil) {
		wait := time.Until(f.lockedUntil).Round(time.Minute)
		m.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Errorf("too many failed logins, try again in %s", wait))
		return
	}
	delay := time.Duration(0)
	if f != nil && f.count >= loginSlowAfter {
		delay = time.Duration(f.count-loginSlowAfter+1) * time.Second
	}
	m.mu.Unlock()
	time.Sleep(delay)

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if !m.checkPassword(req.Username, req.Password) {
		m.mu.Lock()
		f := m.failures[ip]
		if f == nil {
			f = &loginFailures{}
			m.failures[ip] = f
		}
		f.count++
		if f.count >= loginLockAfter {
			f.lockedUntil = time.Now().Add(loginLockout)
			f.count = 0
			log.Printf("login: %s locked out for %s after %d failed attempts", ip, loginLockout, loginLockAfter)
		}
		m.mu.Unlock()
		writeError(w, http.StatusUnauthorized, errors.New("wrong username or password"))
		return
	}

	token := randomToken(32)
	m.mu.Lock()
	delete(m.failures, ip)
	m.sessions[token] = time.Now().Add(sessionLifetime)
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge: int(sessionLifetime.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *authManager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *authManager) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(m.sessions, c.Value)
		return false
	}
	return true
}

// publicPaths are served without a session: the login page and what it
// needs to render.
var publicPaths = map[string]bool{
	"/login.html": true, "/login.js": true, "/style.css": true,
	"/icons/logo-white.svg": true, "/api/login": true, "/api/logout": true,
}

// require wraps the whole GUI: everything except publicPaths needs a
// valid session. API and WebSocket requests get 401; page requests are
// redirected to the login page.
func (m *authManager) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] || m.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
			writeError(w, http.StatusUnauthorized, errors.New("login required"))
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusSeeOther)
	})
}
