package testrun

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"swarmdialer/internal/serverware"
)

// HostMetrics is one sample of the SERVERware host's load (the physical
// node the VPSs run on), from its node exporter.
type HostMetrics struct {
	CPUPct       float64 `json:"cpu_pct"` // average over all cores
	MemPct       float64 `json:"mem_pct"`
	IOWaitPct    float64 `json:"iowait_pct"`
	NetRxBps     float64 `json:"net_rx_bps"`
	NetTxBps     float64 `json:"net_tx_bps"`
	DiskReadBps  float64 `json:"disk_read_bps"`
	DiskWriteBps float64 `json:"disk_write_bps"`
}

// VPSMetrics is one sample of a PBXware VPS's load, from SERVERware's
// per-VPS process metrics. CPU figures are percent of one core.
type VPSMetrics struct {
	CPUPct         float64 `json:"cpu_pct"`
	MemBytes       float64 `json:"mem_bytes"` // proportional resident set (shared memory split fairly)
	AsteriskCPUPct float64 `json:"asterisk_cpu_pct"`
}

// LocalMetrics is SwarmDialer's own load, read from the OS, so a test can
// tell when SwarmDialer itself (not the host) is the bottleneck.
type LocalMetrics struct {
	CPUPct     float64 `json:"cpu_pct"`      // average over all of SwarmDialer's cores
	CPUCorePct float64 `json:"cpu_core_pct"` // same, as percent of one core (like the VPS figures)
	MemPct     float64 `json:"mem_pct"`
	MemBytes   float64 `json:"mem_bytes"`
}

// Monitor samples the host, the PBXware VPSs and SwarmDialer itself.
type Monitor struct {
	sw   *serverware.Client
	node string            // node exporter instance of the active physical node
	vps  map[string]string // role ("MT"/"CC") -> VPS name (its Prometheus instance)

	// HostCPUs (logical CPUs) and HostMemBytes describe the monitored node,
	// so VPS figures can be shown as a share of the whole host.
	HostCPUs     int
	HostMemBytes float64

	mu        sync.Mutex
	lastTotal uint64 // /proc/stat jiffies, for SwarmDialer's own CPU
	lastIdle  uint64
}

// NewMonitor picks the node exporter instance of the physical node that
// hostName's VPSs run on. Node exporter instances are named after the
// host (e.g. "Echo-1" and "Echo-2" for a Mirror pair called "Echo"); on a
// Mirror only the primary node's replication exporter (port 9163) is up,
// which identifies the active node.
func NewMonitor(sw *serverware.Client, hostName string, vps map[string]string) (*Monitor, error) {
	targets, err := sw.PromTargets()
	if err != nil {
		return nil, fmt.Errorf("reading Prometheus targets: %w", err)
	}
	var nodes []string
	replUp := map[string]bool{}
	for _, t := range targets {
		matches := t.Instance == hostName || strings.HasPrefix(t.Instance, hostName+"-")
		if !matches {
			continue
		}
		if t.Port == "9100" && t.Health == "up" {
			nodes = append(nodes, t.Instance)
		}
		if t.Port == "9163" && t.Health == "up" {
			replUp[t.Instance] = true
		}
	}
	sort.Strings(nodes)
	var node string
	switch {
	case len(nodes) == 1:
		node = nodes[0]
	case len(nodes) > 1:
		for _, n := range nodes {
			if replUp[n] {
				node = n
				break
			}
		}
		if node == "" {
			return nil, fmt.Errorf("host %s has several nodes (%s) and none can be identified as the active one", hostName, strings.Join(nodes, ", "))
		}
	default:
		return nil, fmt.Errorf("Prometheus has no host metrics for host %s (no node exporter target named after it)", hostName)
	}
	m := &Monitor{sw: sw, node: node, vps: vps}
	if vals, err := m.queryAll(map[string]string{
		"cpus": fmt.Sprintf(`count(node_cpu_seconds_total{instance=%q,mode="idle"})`, node),
		"mem":  fmt.Sprintf(`node_memory_MemTotal_bytes{instance=%q}`, node),
	}); err == nil {
		m.HostCPUs, m.HostMemBytes = int(vals["cpus"]), vals["mem"]
	}
	m.sampleLocal() // prime the CPU counters
	return m, nil
}

// Node is the node exporter instance being monitored.
func (m *Monitor) Node() string { return m.node }

// SampleHost reads the host's current load.
func (m *Monitor) SampleHost() (HostMetrics, error) {
	n := m.node
	queries := map[string]string{
		"cpu":    fmt.Sprintf(`100 * (1 - avg(rate(node_cpu_seconds_total{instance=%q,mode="idle"}[30s])))`, n),
		"mem":    fmt.Sprintf(`100 * (1 - node_memory_MemAvailable_bytes{instance=%q} / node_memory_MemTotal_bytes{instance=%q})`, n, n),
		"iowait": fmt.Sprintf(`100 * avg(rate(node_cpu_seconds_total{instance=%q,mode="iowait"}[30s]))`, n),
		"rx":     fmt.Sprintf(`sum(rate(node_network_receive_bytes_total{instance=%q,device!~"lo|veth.*|docker.*"}[30s]))`, n),
		"tx":     fmt.Sprintf(`sum(rate(node_network_transmit_bytes_total{instance=%q,device!~"lo|veth.*|docker.*"}[30s]))`, n),
		"dread":  fmt.Sprintf(`sum(rate(node_disk_read_bytes_total{instance=%q}[30s]))`, n),
		"dwrite": fmt.Sprintf(`sum(rate(node_disk_written_bytes_total{instance=%q}[30s]))`, n),
	}
	vals, err := m.queryAll(queries)
	if err != nil {
		return HostMetrics{}, err
	}
	return HostMetrics{
		CPUPct: vals["cpu"], MemPct: vals["mem"], IOWaitPct: vals["iowait"],
		NetRxBps: vals["rx"], NetTxBps: vals["tx"], DiskReadBps: vals["dread"], DiskWriteBps: vals["dwrite"],
	}, nil
}

// SampleVPS reads each PBXware VPS's current load.
func (m *Monitor) SampleVPS() (map[string]VPSMetrics, error) {
	out := map[string]VPSMetrics{}
	for role, name := range m.vps {
		vals, err := m.queryAll(map[string]string{
			"cpu":      fmt.Sprintf(`100 * sum(rate(namedprocess_namegroup_cpu_seconds_total{instance=%q}[30s]))`, name),
			"mem":      fmt.Sprintf(`sum(namedprocess_namegroup_memory_bytes{instance=%q,memtype="proportionalResident"})`, name),
			"asterisk": fmt.Sprintf(`100 * sum(rate(namedprocess_namegroup_cpu_seconds_total{instance=%q,groupname="asterisk"}[30s]))`, name),
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[role] = VPSMetrics{CPUPct: vals["cpu"], MemBytes: vals["mem"], AsteriskCPUPct: vals["asterisk"]}
	}
	return out, nil
}

func (m *Monitor) queryAll(queries map[string]string) (map[string]float64, error) {
	type res struct {
		key string
		val float64
		err error
	}
	ch := make(chan res, len(queries))
	for k, q := range queries {
		go func(k, q string) {
			samples, err := m.sw.PromQuery(q)
			v := 0.0
			if err == nil && len(samples) > 0 {
				v = samples[0].Value
			}
			ch <- res{k, v, err}
		}(k, q)
	}
	out := map[string]float64{}
	var firstErr error
	for range queries {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		out[r.key] = r.val
	}
	return out, firstErr
}

// sampleLocal reads SwarmDialer's own CPU (since the previous call) and
// memory from /proc.
func (m *Monitor) sampleLocal() LocalMetrics {
	var lm LocalMetrics
	if f, err := os.Open("/proc/stat"); err == nil {
		sc := bufio.NewScanner(f)
		if sc.Scan() {
			fields := strings.Fields(sc.Text()) // "cpu user nice system idle iowait irq softirq steal ..."
			var total, idle uint64
			for i, v := range fields[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
				if i == 3 || i == 4 { // idle, iowait
					idle += n
				}
			}
			m.mu.Lock()
			if dt := total - m.lastTotal; m.lastTotal != 0 && dt > 0 {
				lm.CPUPct = 100 * float64(dt-(idle-m.lastIdle)) / float64(dt)
				lm.CPUCorePct = lm.CPUPct * float64(runtime.NumCPU())
			}
			m.lastTotal, m.lastIdle = total, idle
			m.mu.Unlock()
		}
		f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		var total, avail float64
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			v, _ := strconv.ParseFloat(fields[1], 64)
			switch fields[0] {
			case "MemTotal:":
				total = v
			case "MemAvailable:":
				avail = v
			}
		}
		if total > 0 {
			lm.MemPct = 100 * (1 - avail/total)
			lm.MemBytes = (total - avail) * 1024 // meminfo is in KiB
		}
		f.Close()
	}
	return lm
}

// SampleLocal reads SwarmDialer's own load.
func (m *Monitor) SampleLocal() LocalMetrics { return m.sampleLocal() }
