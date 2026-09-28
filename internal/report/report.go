// Package report builds the test report uploaded to DT Collector: report
// format version 1, as agreed in ~/claude/DTcollector_project.md (the DT
// Collector project's brief). Every field is set explicitly from an
// allowlist of values: a report never contains secrets, IP addresses,
// host or VPS names, or hardware serial numbers and UUIDs, only hardware
// details, versions and results.
package report

import (
	"crypto/rand"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"swarmdialer/internal/serverware"
	"swarmdialer/internal/testrun"
)

// SchemaVersion is the report format version.
const SchemaVersion = 1

// SwarmDialerVersion is this SwarmDialer's version, recorded in reports.
const SwarmDialerVersion = "1.4.0"

type Report struct {
	SchemaVersion      int         `json:"schema_version"`
	ReportID           string      `json:"report_id"`
	CreatedAt          time.Time   `json:"created_at"`
	SwarmDialerVersion string      `json:"swarmdialer_version"`
	Profile            Profile     `json:"profile"`
	Environment        Environment `json:"environment"`
	Tests              []Test      `json:"tests"`
}

type Profile struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type Environment struct {
	Serverware Serverware        `json:"serverware"`
	Host       Host              `json:"host"`
	VPS        map[string]VPS    `json:"vps"` // pbxware_mt, pbxware_cc, swarmdialer
	PBXware    []PBXwareInstance `json:"pbxware"`
}

type Serverware struct {
	Version string `json:"version"`
	Edition string `json:"edition"` // standalone | mirror | cluster
}

type Host struct {
	CPUModel     string    `json:"cpu_model"`
	CPUSockets   int       `json:"cpu_sockets"`
	CPUCores     int       `json:"cpu_cores"`
	CPUThreads   int       `json:"cpu_threads"`
	CPUMaxMHz    int       `json:"cpu_max_mhz"`
	MemoryBytes  int64     `json:"memory_bytes"`
	SystemVendor string    `json:"system_vendor,omitempty"`
	SystemModel  string    `json:"system_model,omitempty"`
	Disks        []Disk    `json:"disks"`
	Network      []Network `json:"network"`
}

type Disk struct {
	Model     string `json:"model"`
	SizeBytes int64  `json:"size_bytes"`
	Type      string `json:"type"` // ssd | hdd | nvme | unknown
}

type Network struct {
	SpeedMbps int `json:"speed_mbps"`
}

type VPS struct {
	CPULimit     int `json:"cpu_limit"`
	CPUShare     int `json:"cpu_share,omitempty"`
	MemLimitMB   int `json:"mem_limit_mb"`
	CallrecRAMMB int `json:"callrec_ram_mb,omitempty"`
}

type PBXwareInstance struct {
	Role            string `json:"role"`
	Version         string `json:"version"`
	Edition         string `json:"edition"`
	LicenseChannels int    `json:"license_channels"`
}

type Test struct {
	ID              string     `json:"id"`
	Mode            string     `json:"mode"`
	CallType        string     `json:"call_type"`
	Codec           Codec      `json:"codec"`
	Recording       string     `json:"recording"`
	RecordingFormat string     `json:"recording_format"`
	CallDurationS   int        `json:"call_duration_s"`
	DialRateCPS     float64    `json:"dial_rate_cps"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	Result          Result     `json:"result"`
	Timeseries      Timeseries `json:"timeseries"`
}

type Codec struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
}

type Result struct {
	StopReason             string           `json:"stop_reason"`
	MaxConcurrentCalls     int              `json:"max_concurrent_calls"`
	Calls                  Calls            `json:"calls"`
	SetupMS                SetupMS          `json:"setup_ms"`
	MOS                    MOS              `json:"mos"`
	RTPReceivedRatio       float64          `json:"rtp_received_ratio"`
	QualityDegradedAtCalls *int             `json:"quality_degraded_at_calls"`
	AtTarget               AtTarget         `json:"at_target"`
	Recording              *RecordingResult `json:"recording,omitempty"`
}

type Calls struct {
	Started  int `json:"started"`
	Answered int `json:"answered"`
	Failed   int `json:"failed"`
}

type SetupMS struct {
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

type MOS struct {
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
}

type AtTarget struct {
	HostCPUPct     float64            `json:"host_cpu_pct"`
	HostMemPct     float64            `json:"host_mem_pct"`
	AsteriskCPUPct map[string]float64 `json:"asterisk_cpu_pct"`
}

type RecordingResult struct {
	RAMDiskFullEstimatedAtCalls *int       `json:"ramdisk_full_estimated_at_calls"`
	RAMDiskFullEstimatedAt      *time.Time `json:"ramdisk_full_estimated_at"`
	MP3ConversionDelayS         MP3Delay   `json:"mp3_conversion_delay_s"`
}

type MP3Delay struct {
	Avg   float64 `json:"avg"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
	Trend string  `json:"trend"` // stable | growing
}

type Timeseries struct {
	IntervalS int       `json:"interval_s"`
	Start     time.Time `json:"start"`
	Series    Series    `json:"series"`
}

type Series struct {
	ConcurrentCalls  []int                `json:"concurrent_calls"`
	HostCPUPct       []float64            `json:"host_cpu_pct"`
	HostMemPct       []float64            `json:"host_mem_pct"`
	HostIOWaitPct    []float64            `json:"host_iowait_pct"`
	HostNetRxBps     []float64            `json:"host_net_rx_bps"`
	HostNetTxBps     []float64            `json:"host_net_tx_bps"`
	HostDiskReadBps  []float64            `json:"host_disk_read_bps"`
	HostDiskWriteBps []float64            `json:"host_disk_write_bps"`
	VPSCPUPct        map[string][]float64 `json:"vps_cpu_pct"`   // MT, CC, swarmdialer; % of one core
	VPSMemBytes      map[string][]float64 `json:"vps_mem_bytes"` // MT, CC, swarmdialer
	AsteriskCPUPct   map[string][]float64 `json:"asterisk_cpu_pct"`
	SetupMSP95       []float64            `json:"setup_ms_p95"`
	FailedCalls      []int                `json:"failed_calls"` // calls that failed during each interval
}

// Inputs is everything Build needs besides the run's results.
type Inputs struct {
	Profile    testrun.Profile
	Serverware *serverware.Client
	Node       string // node exporter instance of the tested host (not put in the report)
	Edition    string // SERVERware edition
	VPS        map[string]VPS
	PBXware    []PBXwareInstance
	// CPUModel is the host's CPU model as SERVERware reports it (host
	// platform_details); used as is, so every report names a given CPU
	// the same way and DT Collector can filter by it.
	CPUModel string
}

// Build assembles a report from a finished run.
func Build(in Inputs, results []testrun.Result) (*Report, error) {
	id, err := uuidV4()
	if err != nil {
		return nil, err
	}
	host, err := hostHardware(in.Serverware, in.Node)
	if err != nil {
		return nil, fmt.Errorf("reading host hardware: %w", err)
	}
	if m := strings.TrimSpace(in.CPUModel); m != "" {
		host.CPUModel = m
	}
	r := &Report{
		SchemaVersion: SchemaVersion, ReportID: id, CreatedAt: time.Now().UTC(),
		SwarmDialerVersion: SwarmDialerVersion,
		Profile:            Profile{Name: in.Profile.Name, Version: in.Profile.Version},
		Environment: Environment{
			Serverware: Serverware{Version: serverwareVersion(in.Serverware), Edition: in.Edition},
			Host:       host, VPS: in.VPS, PBXware: in.PBXware,
		},
	}
	for _, res := range results {
		r.Tests = append(r.Tests, buildTest(res))
	}
	return r, nil
}

func buildTest(res testrun.Result) Test {
	t := res.Test
	format := t.RecordingFormat
	if t.Recording == testrun.RecordingStereo {
		format = "wav" // PBXware's stereo recording format
	} else if t.Recording == testrun.RecordingOff {
		format = ""
	}
	rate := 0.0
	if t.Mode == testrun.ModeRamp && t.DialInterval > 0 {
		rate = round2(float64(time.Second) / float64(t.DialInterval))
	}
	for _, c := range t.RollingCPS {
		rate = math.Max(rate, c)
	}
	out := Test{
		ID: t.ID, Mode: t.Mode, CallType: "remote",
		Codec:     Codec{Caller: t.CallerCodec, Callee: t.CalleeCodec},
		Recording: t.Recording, RecordingFormat: format,
		CallDurationS: int(t.CallDuration.Seconds()), DialRateCPS: rate,
		StartedAt: res.StartedAt.UTC(), FinishedAt: res.FinishedAt.UTC(),
		Result: Result{
			StopReason:             res.StopReason,
			MaxConcurrentCalls:     res.MaxConcurrent,
			Calls:                  Calls{Started: res.Calls.Started, Answered: res.Calls.Answered, Failed: res.Calls.Failed},
			SetupMS:                SetupMS{Avg: res.SetupMS.Avg, P95: res.SetupMS.P95, Max: res.SetupMS.Max},
			MOS:                    MOS{Avg: res.MOS.Avg, Min: res.MOS.Min},
			RTPReceivedRatio:       res.RTPReceivedRatio,
			QualityDegradedAtCalls: res.QualityDegradedAtCalls,
			AtTarget: AtTarget{
				HostCPUPct: res.AtLoad.HostCPUPct, HostMemPct: res.AtLoad.HostMemPct,
				AsteriskCPUPct: res.AtLoad.AsteriskCPUPct,
			},
		},
	}
	if rec := res.Recording; rec != nil {
		rr := &RecordingResult{RAMDiskFullEstimatedAtCalls: rec.RAMDiskFullEstAtCalls, MP3ConversionDelayS: MP3Delay{Trend: "stable"}}
		if rec.RAMDiskFullEstAt != nil {
			at := rec.RAMDiskFullEstAt.UTC()
			rr.RAMDiskFullEstimatedAt = &at
		}
		// One figure for both instances: the slower one.
		for role, st := range rec.MP3ConversionDelayS {
			rr.MP3ConversionDelayS.Avg = math.Max(rr.MP3ConversionDelayS.Avg, st.Avg)
			rr.MP3ConversionDelayS.P95 = math.Max(rr.MP3ConversionDelayS.P95, st.P95)
			rr.MP3ConversionDelayS.Max = math.Max(rr.MP3ConversionDelayS.Max, st.Max)
			if rec.MP3DelayTrend[role] == "growing" {
				rr.MP3ConversionDelayS.Trend = "growing"
			}
		}
		out.Result.Recording = rr
	}

	ser := Series{
		VPSCPUPct:      map[string][]float64{"MT": {}, "CC": {}, "swarmdialer": {}},
		VPSMemBytes:    map[string][]float64{"MT": {}, "CC": {}, "swarmdialer": {}},
		AsteriskCPUPct: map[string][]float64{"MT": {}, "CC": {}},
	}
	var lastFailed uint64
	for i, s := range res.Samples {
		ser.ConcurrentCalls = append(ser.ConcurrentCalls, s.ConcurrentCalls)
		ser.HostCPUPct = append(ser.HostCPUPct, round2(s.Host.CPUPct))
		ser.HostMemPct = append(ser.HostMemPct, round2(s.Host.MemPct))
		ser.HostIOWaitPct = append(ser.HostIOWaitPct, round2(s.Host.IOWaitPct))
		ser.HostNetRxBps = append(ser.HostNetRxBps, math.Round(s.Host.NetRxBps))
		ser.HostNetTxBps = append(ser.HostNetTxBps, math.Round(s.Host.NetTxBps))
		ser.HostDiskReadBps = append(ser.HostDiskReadBps, math.Round(s.Host.DiskReadBps))
		ser.HostDiskWriteBps = append(ser.HostDiskWriteBps, math.Round(s.Host.DiskWriteBps))
		for _, role := range []string{"MT", "CC"} {
			v := s.VPS[role]
			ser.VPSCPUPct[role] = append(ser.VPSCPUPct[role], round2(v.CPUPct))
			ser.VPSMemBytes[role] = append(ser.VPSMemBytes[role], math.Round(v.MemBytes))
			ser.AsteriskCPUPct[role] = append(ser.AsteriskCPUPct[role], round2(v.AsteriskCPUPct))
		}
		ser.VPSCPUPct["swarmdialer"] = append(ser.VPSCPUPct["swarmdialer"], round2(s.Local.CPUCorePct))
		ser.VPSMemBytes["swarmdialer"] = append(ser.VPSMemBytes["swarmdialer"], math.Round(s.Local.MemBytes))
		ser.SetupMSP95 = append(ser.SetupMSP95, float64(s.SetupP95MS))
		failed := 0
		if i > 0 && s.Failed >= lastFailed {
			failed = int(s.Failed - lastFailed)
		}
		lastFailed = s.Failed
		ser.FailedCalls = append(ser.FailedCalls, failed)
	}
	out.Timeseries = Timeseries{IntervalS: 5, Series: ser}
	if len(res.Samples) > 0 {
		out.Timeseries.Start = res.Samples[0].T.UTC()
	}
	return out
}

// hostHardware reads the tested node's hardware from its node exporter.
// Only descriptive details are kept: serial numbers, UUIDs, asset tags,
// host names and device paths never go into a report.
func hostHardware(sw *serverware.Client, node string) (Host, error) {
	h := Host{CPUModel: "unknown"} // replaced by SERVERware's platform_details (see Build); node exporter doesn't report it
	num := func(q string) float64 {
		s, err := sw.PromQuery(q)
		if err != nil || len(s) == 0 {
			return 0
		}
		return s[0].Value
	}
	sel := fmt.Sprintf(`{instance=%q}`, node)
	h.CPUThreads = int(num(`count(node_cpu_seconds_total{mode="idle",instance="` + node + `"})`))
	if h.CPUThreads == 0 {
		return h, fmt.Errorf("no CPU metrics for the host")
	}
	h.CPUSockets = int(num(`count(count by (package) (node_cpu_core_throttles_total` + sel + `))`))
	if cores := int(num(`count(count by (package, core) (node_cpu_core_throttles_total` + sel + `))`)); cores > 0 {
		h.CPUCores = cores
	} else {
		h.CPUCores = h.CPUThreads
	}
	h.CPUMaxMHz = int(num(`max(node_cpu_frequency_max_hertz`+sel+`)`) / 1e6)
	h.MemoryBytes = int64(num(`node_memory_MemTotal_bytes` + sel))

	if s, err := sw.PromQuery(`node_dmi_info` + sel); err == nil && len(s) > 0 {
		h.SystemVendor, h.SystemModel = cleanDMI(s[0].Labels["system_vendor"]), cleanDMI(s[0].Labels["product_name"])
	}

	nvme := map[string]bool{}
	if s, err := sw.PromQuery(`node_nvme_info` + sel); err == nil {
		for _, x := range s {
			nvme[x.Labels["device"]] = true
		}
	}
	h.Disks = []Disk{}
	if s, err := sw.PromQuery(`node_disk_info` + sel); err == nil {
		sort.Slice(s, func(i, j int) bool { return s[i].Labels["device"] < s[j].Labels["device"] })
		for _, x := range s {
			dev, model := x.Labels["device"], strings.ReplaceAll(x.Labels["model"], "_", " ")
			if model == "" || model == "Linux" || strings.HasPrefix(dev, "zd") || strings.HasPrefix(dev, "dm-") || strings.HasPrefix(dev, "loop") {
				// Virtual devices. Model "Linux" is a Linux NVMe-over-TCP
				// target: network storage volumes, not the host's own disks.
				continue
			}
			typ := "unknown"
			switch {
			case strings.HasPrefix(dev, "nvme") || nvme[dev]:
				typ = "nvme"
			case strings.Contains(strings.ToUpper(model), "SSD") || strings.HasPrefix(strings.ToUpper(model), "SAMSUNG MZ"):
				typ = "ssd"
			}
			h.Disks = append(h.Disks, Disk{Model: model, Type: typ})
		}
	}
	h.Network = []Network{}
	if s, err := sw.PromQuery(`node_network_speed_bytes` + sel); err == nil {
		sort.Slice(s, func(i, j int) bool { return s[i].Labels["device"] < s[j].Labels["device"] })
		for _, x := range s {
			dev := x.Labels["device"]
			// Physical ports only: not bridges, bonds or virtual links.
			if !(strings.HasPrefix(dev, "en") || strings.HasPrefix(dev, "eth")) || x.Value <= 0 {
				continue
			}
			h.Network = append(h.Network, Network{SpeedMbps: int(x.Value * 8 / 1e6)})
		}
	}
	return h, nil
}

func cleanDMI(v string) string {
	if strings.Contains(strings.ToLower(v), "to be filled") {
		return ""
	}
	return strings.TrimSpace(v)
}

// serverwareVersion reads SERVERware's version from the SRW exporter,
// trimmed to "major.minor.patch".
func serverwareVersion(sw *serverware.Client) string {
	s, err := sw.PromQuery("srw_info")
	if err != nil || len(s) == 0 {
		return "unknown"
	}
	v := s[0].Labels["version"]
	if i := strings.IndexAny(v, "+-"); i > 0 {
		v = v[:i]
	}
	return v
}

// Edition works out the SERVERware edition from its hosts.
func Edition(hosts []serverware.Host) string {
	switch {
	case len(hosts) > 1:
		return "cluster"
	case len(hosts) == 1 && hosts[0].MirrorID > 0:
		return "mirror"
	default:
		return "standalone"
	}
}

func uuidV4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
