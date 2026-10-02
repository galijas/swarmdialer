package wizard

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmdialer/internal/serverware"
	"swarmdialer/internal/store"
)

// ServerwareProgress reports live progress of ConnectServerware — same
// mutex-guarded snapshot pattern as ProvisionProgress.
type ServerwareProgress struct {
	mu       sync.Mutex
	steps    []string // completed steps, shown as a checklist
	message  string   // the step in progress
	warnings []string
	done     bool
	err      string
	// restart lists the PBXware VPSs whose recording RAM disk was raised
	// and that need a restart for it to take effect.
	restart []RAMDiskRestart
}

// RAMDiskRestart is a PBXware VPS whose recording RAM disk was raised in
// SERVERware and that needs a restart for it to take effect.
type RAMDiskRestart struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`     // the PBXware instance's name
	VPSName  string `json:"vps_name"` // its VPS in SERVERware
	FromMB   int    `json:"from_mb"`
}

func (p *ServerwareProgress) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// Finish marks the job successfully done.
func (p *ServerwareProgress) Finish() {
	p.set(func() { p.done = true; p.message = "" })
}

// Fail marks the job failed after ConnectServerware returned (e.g. the
// result couldn't be saved).
func (p *ServerwareProgress) Fail(err error) {
	p.set(func() { p.done = true; p.err = err.Error(); p.message = "" })
}

func (p *ServerwareProgress) step(msg string) { p.set(func() { p.message = msg }) }

func (p *ServerwareProgress) completed(msg string) {
	p.set(func() { p.steps = append(p.steps, msg); p.message = "" })
}

// ServerwareProgressSnapshot is a serializable copy of a ServerwareProgress.
type ServerwareProgressSnapshot struct {
	Steps    []string `json:"steps"`
	Message  string   `json:"message"`
	Warnings []string `json:"warnings,omitempty"`
	Done     bool     `json:"done"`
	Err      string   `json:"error,omitempty"`
	// RestartNeeded is set when connecting raised a RAM disk (see
	// RAMDiskRestart); the GUI then offers to restart those VPSs.
	RestartNeeded []RAMDiskRestart `json:"restart_needed,omitempty"`
}

// Snapshot returns a copy safe to read or serialize without locking.
func (p *ServerwareProgress) Snapshot() ServerwareProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ServerwareProgressSnapshot{
		Steps: slices.Clone(p.steps), Message: p.message, Warnings: slices.Clone(p.warnings),
		Done: p.done, Err: p.err, RestartNeeded: slices.Clone(p.restart),
	}
}

// ServerwareParams configures ConnectServerware.
type ServerwareParams struct {
	Controller string // IP, host name or URL of the SERVERware controller
	APIKey     string // SERVERware admin API token
	// Servers are the connected PBXware instances; each must run in a VPS
	// on this SERVERware.
	Servers []*store.Server
	// SelfIP is SwarmDialer's own address (the one it uses toward the
	// PBXware instances), used to find SwarmDialer's own VPS.
	SelfIP string
}

// How long to wait for SERVERware to apply settings and for Prometheus to
// start collecting the new metrics. Live, per-VPS metrics appeared about
// 20s after enabling them.
const (
	collectMetricsApplyWait = time.Minute
	promTargetWait          = 3 * time.Minute
)

// ConnectServerware validates and sets up host monitoring on a SERVERware
// site: checks the API key, finds the VPSs of SwarmDialer and the PBXware
// instances by IP, checks they all run on the same host (the test
// measures one host's hardware, so all three must share it), turns on
// observability and per-VPS metrics for the PBXware VPSs, waits until
// Prometheus is collecting them, and raises the PBXware VPSs' call
// recording RAM disk to ramDiskSizeMB (those needing a restart for it are
// listed in the progress, see RAMDiskRestart). SwarmDialer's own VPS is left alone: it
// can be KVM, where SERVERware only applies some edits while the VPS is
// stopped, and SwarmDialer measures its own load directly instead.
func ConnectServerware(p ServerwareParams, progress *ServerwareProgress) (*store.Serverware, error) {
	fail := func(err error) (*store.Serverware, error) {
		progress.set(func() { progress.done = true; progress.err = err.Error(); progress.message = "" })
		return nil, err
	}

	sw := serverware.NewClient(p.Controller, p.APIKey)

	progress.step("checking the SERVERware API key")
	hosts, err := sw.Hosts()
	if err != nil {
		if serverware.IsUnauthorized(err) {
			return fail(fmt.Errorf("SERVERware rejected the API key (it must be an admin API token)"))
		}
		return fail(fmt.Errorf("connecting to SERVERware at %s: %w", sw.BaseURL, err))
	}
	progress.completed(fmt.Sprintf("connected to SERVERware at %s", sw.BaseURL))

	progress.step("finding the VPSs of SwarmDialer and the PBXware instances")
	vpses, err := sw.VPSes()
	if err != nil {
		return fail(fmt.Errorf("listing VPSs: %w", err))
	}
	findByIP := func(ip string) *serverware.VPS {
		for i := range vpses {
			if slices.Contains(vpses[i].IPAddresses, ip) {
				return &vpses[i]
			}
		}
		return nil
	}
	self := findByIP(p.SelfIP)
	if self == nil {
		return fail(fmt.Errorf("SwarmDialer's own VPS (IP %s) isn't on this SERVERware. SwarmDialer, and both PBXware instances, must all run on the SERVERware host being tested", p.SelfIP))
	}
	type found struct {
		srv *store.Server
		vps *serverware.VPS
	}
	var pbx []found
	for _, srv := range p.Servers {
		ip, err := hostIP(srv.BaseURL)
		if err != nil {
			return fail(fmt.Errorf("resolving %s's address: %w", srv.Name, err))
		}
		v := findByIP(ip)
		if v == nil {
			return fail(fmt.Errorf("the VPS of %s (IP %s) isn't on this SERVERware. SwarmDialer and both PBXware instances must all run on the SERVERware host being tested", srv.Name, ip))
		}
		pbx = append(pbx, found{srv, v})
	}
	names := []string{self.Name}
	for _, f := range pbx {
		names = append(names, f.vps.Name)
	}
	progress.completed("found VPSs: " + strings.Join(names, ", "))

	progress.step("checking that all VPSs run on the same host")
	hostName := func(id int) string {
		for _, h := range hosts {
			if h.ID == id {
				return h.Name
			}
		}
		return fmt.Sprintf("host %d", id)
	}
	var elsewhere []string
	for _, f := range pbx {
		if f.vps.HostID != self.HostID {
			elsewhere = append(elsewhere, fmt.Sprintf("%s is on %s", f.vps.Name, hostName(f.vps.HostID)))
		}
	}
	if len(elsewhere) > 0 {
		return fail(fmt.Errorf("the test measures one host, so SwarmDialer and both PBXware VPSs must run on the same host. %s is on %s, but %s. Move them to one host (in SERVERware: VPSs, Change host), then connect again",
			self.Name, hostName(self.HostID), strings.Join(elsewhere, ", ")))
	}
	var host serverware.Host
	for _, h := range hosts {
		if h.ID == self.HostID {
			host = h
		}
	}
	progress.completed(fmt.Sprintf("all VPSs are on host %s", host.Name))

	progress.step("checking observability")
	on, err := sw.ObservabilityEnabled()
	if err != nil {
		return fail(fmt.Errorf("reading the observability setting: %w", err))
	}
	if !on {
		progress.step("turning on observability")
		if err := sw.EnableObservability(); err != nil {
			return fail(fmt.Errorf("turning on observability: %w", err))
		}
	}
	progress.step("waiting for Prometheus to collect SERVERware metrics (SRW exporter)")
	if err := waitForTarget(sw, func(t serverware.PromTarget) bool { return t.Port == "9101" }, promTargetWait); err != nil {
		return fail(fmt.Errorf("observability is on, but %w", err))
	}
	if on {
		progress.completed("observability is on")
	} else {
		progress.completed("observability turned on")
	}

	for _, f := range pbx {
		name := f.vps.Name
		if f.vps.Engine != "lxc" {
			progress.set(func() {
				progress.warnings = append(progress.warnings, fmt.Sprintf("%s is a %s VPS, so per-VPS metrics weren't turned on (only LXC was tested); its host metrics are still collected", name, f.vps.Engine))
			})
			continue
		}
		progress.step(fmt.Sprintf("turning on Collect Metrics for %s", name))
		if err := sw.EnableCollectMetrics(f.vps.ID, collectMetricsApplyWait); err != nil {
			return fail(fmt.Errorf("turning on Collect Metrics for %s: %w", name, err))
		}
		progress.step(fmt.Sprintf("waiting for Prometheus to collect %s's metrics", name))
		if err := waitForTarget(sw, func(t serverware.PromTarget) bool { return t.Instance == name }, promTargetWait); err != nil {
			return fail(fmt.Errorf("Collect Metrics is on for %s, but %w", name, err))
		}
		progress.completed(fmt.Sprintf("collecting metrics for %s", name))
	}

	// Call recording RAM disk: on SERVERware, the PBXware VPS's tmpfs comes
	// from the VPS's callrec_ram_mb, not from PBXware's own setting (which
	// provisioning already set), and is created when the VPS starts.
	for _, f := range pbx {
		name := f.vps.Name
		if f.vps.Engine != "lxc" {
			progress.set(func() {
				progress.warnings = append(progress.warnings, fmt.Sprintf("%s is a %s VPS, so its recording RAM disk wasn't checked (only LXC was tested); make sure it is at least %d MB for stereo recording", name, f.vps.Engine, ramDiskSizeMB))
			})
			continue
		}
		v, err := sw.GetVPS(f.vps.ID)
		if err != nil {
			return fail(fmt.Errorf("reading %s: %w", name, err))
		}
		if v.CallrecRAMMB >= ramDiskSizeMB {
			progress.completed(fmt.Sprintf("recording RAM disk of %s is %d MB", name, v.CallrecRAMMB))
			continue
		}
		progress.step(fmt.Sprintf("raising the recording RAM disk of %s to %d MB", name, ramDiskSizeMB))
		if err := sw.SetCallrecRAM(f.vps.ID, ramDiskSizeMB, collectMetricsApplyWait); err != nil {
			return fail(fmt.Errorf("raising the recording RAM disk of %s: %w", name, err))
		}
		from, srv := v.CallrecRAMMB, f.srv
		progress.set(func() {
			progress.steps = append(progress.steps, fmt.Sprintf("raised the recording RAM disk of %s from %d to %d MB (takes effect after a VPS restart)", name, from, ramDiskSizeMB))
			progress.message = ""
			progress.restart = append(progress.restart, RAMDiskRestart{ServerID: srv.ID, Name: srv.Name, VPSName: name, FromMB: from})
		})
	}

	cfg := &store.Serverware{
		ControllerURL: sw.BaseURL, APIKey: sw.Token,
		HostID: host.ID, HostUUID: host.UUID, HostName: host.Name,
		PBXwareVPS:     map[string]store.VPSRef{},
		SwarmDialerVPS: store.VPSRef{ID: self.ID, UUID: self.UUID, Name: self.Name},
	}
	for _, f := range pbx {
		cfg.PBXwareVPS[f.srv.ID] = store.VPSRef{ID: f.vps.ID, UUID: f.vps.UUID, Name: f.vps.Name}
	}
	// Not marked done here: the caller marks it done (Finish) once it has
	// saved cfg, so nothing reads the saved connection before it exists.
	return cfg, nil
}

// waitForTarget waits until Prometheus reports a healthy target matching
// match.
func waitForTarget(sw *serverware.Client, match func(serverware.PromTarget) bool, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		targets, err := sw.PromTargets()
		if err == nil {
			for _, t := range targets {
				if match(t) && t.Health == "up" {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("Prometheus couldn't be read after %s: %w", wait, err)
			}
			return fmt.Errorf("Prometheus still isn't collecting it after %s", wait)
		}
		time.Sleep(5 * time.Second)
	}
}

// hostIP returns the IP address of a PBXware base URL's host.
func hostIP(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return host, nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", err
	}
	return addrs[0], nil
}
