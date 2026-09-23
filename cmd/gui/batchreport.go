package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"swarmdialer/internal/logstore"
	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
)

// mosFinalizeDelay is a courtesy wait before querying MOS — PBXware
// writes/finalizes a call's CDR (and the MOS data attached to it) shortly
// after the call ends, not instantly; querying too soon just means "no
// data yet" for calls that do have real data moments later (confirmed
// live 2026-09-23).
const mosFinalizeDelay = 8 * time.Second

// chunkSizeFor picks how many calls go in each MOS/latency aggregate
// chunk for a batch of n calls, per the two reference points given for
// this feature: 50 calls -> chunks of 25, 500 calls -> chunks of 100.
// Both fit size = round(n/5) to the nearest 25 (floor 25), which is what
// this computes; n<=25 is never split (a single chunk covering the whole
// batch).
func chunkSizeFor(n int) int {
	if n <= 25 {
		return n
	}
	size := n / 5
	size = ((size + 12) / 25) * 25
	if size < 25 {
		size = 25
	}
	return size
}

type floatStats struct {
	Count         int
	Min, Avg, Max float64
}

func summarizeFloats(values []float64) (floatStats, bool) {
	if len(values) == 0 {
		return floatStats{}, false
	}
	sum, min, max := 0.0, values[0], values[0]
	for _, v := range values {
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return floatStats{Count: len(values), Min: min, Avg: sum / float64(len(values)), Max: max}, true
}

type latencyStats struct {
	Count         int
	Min, Avg, P95 time.Duration
}

func summarizeLatencies(values []time.Duration) (latencyStats, bool) {
	if len(values) == 0 {
		return latencyStats{}, false
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	p95Idx := int(float64(len(sorted)) * 0.95)
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}
	return latencyStats{
		Count: len(sorted), Min: sorted[0], Avg: sum / time.Duration(len(sorted)), P95: sorted[p95Idx],
	}, true
}

func formatFloatStats(label string, s floatStats, ok bool) string {
	if !ok {
		return fmt.Sprintf("%s: no data", label)
	}
	return fmt.Sprintf("%s: avg=%.2f min=%.2f max=%.2f (n=%d)", label, s.Avg, s.Min, s.Max, s.Count)
}

func formatLatencyStats(label string, s latencyStats, ok bool) string {
	if !ok {
		return fmt.Sprintf("%s: no data", label)
	}
	return fmt.Sprintf("%s: avg=%s min=%s p95=%s (n=%d)", label,
		s.Avg.Round(time.Millisecond), s.Min.Round(time.Millisecond), s.P95.Round(time.Millisecond), s.Count)
}

// callLogEntry is one call's full detail for logging/reporting — a
// CallRecord plus the MOS data looked up for it (nil if none is
// available — see reportBatch).
type callLogEntry struct {
	orchestrator.CallRecord
	MOS    pbxware.CallMOS
	HasMOS bool
}

// reportBatch runs once a dial batch finishes (see handleDial's AddCalls
// call): after giving PBXware a moment to finalize CDRs (see
// mosFinalizeDelay), it looks up MOS for every answered call, writes one
// full log file (every call's detail, MOS included, plus a summary), and
// posts a follow-up "batch_quality" event (latency + MOS, both broken
// into chunks for large batches — see chunkSizeFor) to the session's
// event log via LogBatchQuality — shortly after AddCalls' own immediate
// "batch_done" event, once this is all ready.
//
// client/serverID identify which PBXware instance to query CDRs/MOS
// against — always the *caller's* instance (for a remote batch, that's
// server1's own API, not the callee's — a cross-instance call's CDR is
// recorded on the side that originated it).
func (a *app) reportBatch(section logstore.Section, label string, client *pbxware.Client, serverID int, sess *orchestrator.Session, requestedN int, records []orchestrator.CallRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].StartedAt.Before(records[j].StartedAt) })

	entries := make([]callLogEntry, len(records))
	for i, r := range records {
		entries[i] = callLogEntry{CallRecord: r}
	}

	answeredIdx := make([]int, 0, len(entries))
	for i, e := range entries {
		if e.Answered {
			answeredIdx = append(answeredIdx, i)
		}
	}
	if len(answeredIdx) > 0 {
		time.Sleep(mosFinalizeDelay)

		windows := make([]pbxware.CallWindow, len(answeredIdx))
		for j, i := range answeredIdx {
			windows[j] = pbxware.CallWindow{Ext: entries[i].CallerAOR, Start: entries[i].StartedAt, End: entries[i].EndedAt}
		}
		mosByWindow := client.BatchGetMOS(serverID, windows)
		for j, i := range answeredIdx {
			if m, ok := mosByWindow[j]; ok {
				entries[i].MOS, entries[i].HasMOS = m, true
			}
		}
	}

	a.writeBatchLog(section, label, requestedN, entries)
	sess.LogBatchQuality(batchQualitySummary(entries))
}

// batchQualitySummary formats the overall + per-chunk latency/MOS
// aggregates as a single log line (see chunkSizeFor for the chunking
// rule).
func batchQualitySummary(entries []callLogEntry) string {
	records := make([]orchestrator.CallRecord, len(entries))
	for i, e := range entries {
		records[i] = e.CallRecord
	}

	var lines []string
	overallLatency, _ := summarizeLatencies(latenciesOf(records))
	lines = append(lines, formatLatencyStats("latency", overallLatency, true))

	var allMOS []float64
	for _, e := range entries {
		if e.HasMOS {
			allMOS = append(allMOS, e.MOS.Avg)
		}
	}
	overallMOS, haveMOS := summarizeFloats(allMOS)
	lines = append(lines, formatFloatStats("mos", overallMOS, haveMOS))

	if size := chunkSizeFor(len(entries)); size < len(entries) {
		for start := 0; start < len(entries); start += size {
			end := start + size
			if end > len(entries) {
				end = len(entries)
			}
			chunk := entries[start:end]
			chunkRecords := make([]orchestrator.CallRecord, len(chunk))
			var chunkMOS []float64
			for i, e := range chunk {
				chunkRecords[i] = e.CallRecord
				if e.HasMOS {
					chunkMOS = append(chunkMOS, e.MOS.Avg)
				}
			}
			chunkLatency, _ := summarizeLatencies(latenciesOf(chunkRecords))
			mosStat, mosOK := summarizeFloats(chunkMOS)
			lines = append(lines, fmt.Sprintf("calls %d-%d: %s | %s",
				start+1, end,
				formatLatencyStats("latency", chunkLatency, true),
				formatFloatStats("mos", mosStat, mosOK)))
		}
	}

	return strings.Join(lines, "; ")
}

func latenciesOf(records []orchestrator.CallRecord) []time.Duration {
	out := make([]time.Duration, len(records))
	for i, r := range records {
		out[i] = r.SetupLatency
	}
	return out
}

// writeBatchLog writes one full log file for a completed batch: every
// call's detail (including MOS, if available) followed by a summary —
// matching what's shown in Live Status, plus extra per-call fields worth
// having in a log even though the dashboard doesn't show them live
// (exact timestamps, codec, RTP counts).
func (a *app) writeBatchLog(section logstore.Section, label string, requestedN int, entries []callLogEntry) {
	f, _, err := a.logs.CreateBatchLog(section, label)
	if err != nil {
		return // logging is best-effort — never block/fail the actual call batch over it
	}
	defer f.Close()

	answered, failed := 0, 0
	fmt.Fprintf(f, "SwarmDialer batch log\n")
	fmt.Fprintf(f, "Section: %s\n", section)
	fmt.Fprintf(f, "Target:  %s\n", label)
	fmt.Fprintf(f, "Requested: %d calls, started: %d\n\n", requestedN, len(entries))
	fmt.Fprintf(f, "=== Calls ===\n")
	for _, e := range entries {
		mosStr := "mos=n/a"
		if e.HasMOS {
			mosStr = fmt.Sprintf("mos avg=%.2f min=%.2f max=%.2f", e.MOS.Avg, e.MOS.Min, e.MOS.Max)
		}
		if e.Answered {
			answered++
			fmt.Fprintf(f, "[%s] %s -> %s  section=%s  codec=%s  ANSWERED  setup=%s  rtp sent=%d recv=%d  %s\n",
				e.StartedAt.UTC().Format("15:04:05.000"), e.CallerAOR, e.CalleeAOR, section, e.Codec,
				e.SetupLatency.Round(time.Millisecond), e.RTPSent, e.RTPRecv, mosStr)
		} else {
			failed++
			fmt.Fprintf(f, "[%s] %s -> %s  section=%s  codec=%s  FAILED (%s)  setup=%s\n",
				e.StartedAt.UTC().Format("15:04:05.000"), e.CallerAOR, e.CalleeAOR, section, e.Codec,
				e.FailReason, e.SetupLatency.Round(time.Millisecond))
		}
	}

	fmt.Fprintf(f, "\n")
	writeSummarySection(f, len(entries), answered, failed, entries)
}

// writeSummarySection writes the log file's summary block — unlike
// batchQualitySummary (a single compact line for the live-status event,
// read in the moment on the dashboard), this is read after the fact, often
// much later, so it spells out what each stat means (MOS's scale, what
// p95 tells you that a plain average doesn't, how many calls each number
// is actually based on) instead of assuming the reader remembers the
// dashboard's shorthand.
func writeSummarySection(f *os.File, started, answered, failed int, entries []callLogEntry) {
	fmt.Fprintf(f, "=== Summary ===\n\n")
	fmt.Fprintf(f, "Calls started: %d\n", started)
	fmt.Fprintf(f, "Answered:      %d\n", answered)
	fmt.Fprintf(f, "Failed:        %d\n\n", failed)

	records := make([]orchestrator.CallRecord, len(entries))
	for i, e := range entries {
		records[i] = e.CallRecord
	}

	fmt.Fprintf(f, "--- Call setup latency (time from sending the INVITE to getting an answer) ---\n")
	if overall, ok := summarizeLatencies(latenciesOf(records)); ok {
		fmt.Fprintf(f, "Average:                %s\n", overall.Avg.Round(time.Millisecond))
		fmt.Fprintf(f, "Minimum:                %s\n", overall.Min.Round(time.Millisecond))
		fmt.Fprintf(f, "95th percentile (p95):  %s  (95%% of these %d calls set up faster than this — a steadier read on the slow end than the average, which one outlier can skew)\n\n",
			overall.P95.Round(time.Millisecond), overall.Count)
	} else {
		fmt.Fprintf(f, "No data — no calls were started.\n\n")
	}

	var allMOS []float64
	for _, e := range entries {
		if e.HasMOS {
			allMOS = append(allMOS, e.MOS.Avg)
		}
	}
	fmt.Fprintf(f, "--- Call quality (MOS — Mean Opinion Score, a standard 1.0-5.0 audio quality scale, higher is better) ---\n")
	if overall, ok := summarizeFloats(allMOS); ok {
		fmt.Fprintf(f, "Average: %.2f\n", overall.Avg)
		fmt.Fprintf(f, "Minimum: %.2f\n", overall.Min)
		fmt.Fprintf(f, "Maximum: %.2f\n", overall.Max)
		fmt.Fprintf(f, "Based on %d of %d calls — only answered calls have MOS data.\n\n", overall.Count, started)
	} else {
		fmt.Fprintf(f, "No data — none of these calls were answered.\n\n")
	}

	size := chunkSizeFor(len(entries))
	if size >= len(entries) {
		return
	}
	fmt.Fprintf(f, "--- Breakdown by chunk of %d calls ---\n", size)
	fmt.Fprintf(f, "(large batches are split into chunks like this so you can see quality/latency trends across the batch, instead of one blended average)\n\n")
	for start := 0; start < len(entries); start += size {
		end := start + size
		if end > len(entries) {
			end = len(entries)
		}
		chunk := entries[start:end]
		chunkRecords := make([]orchestrator.CallRecord, len(chunk))
		var chunkMOS []float64
		for i, e := range chunk {
			chunkRecords[i] = e.CallRecord
			if e.HasMOS {
				chunkMOS = append(chunkMOS, e.MOS.Avg)
			}
		}
		chunkLatency, _ := summarizeLatencies(latenciesOf(chunkRecords))
		mosStat, mosOK := summarizeFloats(chunkMOS)
		fmt.Fprintf(f, "Calls %d-%d:\n", start+1, end)
		fmt.Fprintf(f, "  Latency — average %s, minimum %s, p95 %s\n",
			chunkLatency.Avg.Round(time.Millisecond), chunkLatency.Min.Round(time.Millisecond), chunkLatency.P95.Round(time.Millisecond))
		if mosOK {
			fmt.Fprintf(f, "  MOS     — average %.2f, minimum %.2f, maximum %.2f (%d of %d calls answered)\n\n", mosStat.Avg, mosStat.Min, mosStat.Max, mosStat.Count, len(chunk))
		} else {
			fmt.Fprintf(f, "  MOS     — no data (none of these calls were answered)\n\n")
		}
	}
}
