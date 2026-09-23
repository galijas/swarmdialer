package pbxware

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// cdrEntry is one row of pbxware.cdr.download's response, trimmed to what
// MOS lookup needs.
type cdrEntry struct {
	from      string
	startUnix int64
	uniqueID  string
}

// CallMOS is one call's MOS quality data, as read back via BatchGetMOS.
type CallMOS struct {
	Max, Avg, Min float64
}

// CallWindow identifies one call to look up MOS for: the extension that
// originated it (matches pbxware.cdr.download's "ext" filter — i.e. the
// caller, not the callee) and its expected start/end time.
type CallWindow struct {
	Ext        string
	Start, End time.Time
}

// BatchGetMOS looks up MOS for many calls at once — built for SwarmDialer's
// batch sizes (up to ~1000 calls), where looking up each call individually
// (a CDR search plus a MOS query per call) would mean thousands of PBXware
// API round-trips. Instead this does a small, fixed number of
// pbxware.cdr.download calls (one per relevant calendar day, across every
// extension in windows at once, via the API's documented comma-separated
// ext filter) to resolve every call's CDR Unique ID locally, and only then
// makes one pbxware.cdr.mos call per call that actually needs one.
//
// The returned map is keyed by windows' index; a call missing from it
// simply has no MOS data yet (not an error — see GetCallMOS's doc comment
// for why that's a normal, expected outcome for some calls).
func (c *Client) BatchGetMOS(serverID int, windows []CallWindow) map[int]CallMOS {
	result := make(map[int]CallMOS, len(windows))
	if len(windows) == 0 {
		return result
	}

	uniqueIDs := c.batchFindCDRUniqueIDs(serverID, windows)
	for i, uniqueID := range uniqueIDs {
		if uniqueID == "" {
			continue
		}
		mos, ok, err := c.getMOSByUniqueID(uniqueID)
		if err != nil || !ok {
			continue
		}
		result[i] = mos
	}
	return result
}

// batchFindCDRUniqueIDs resolves every window's CDR Unique ID with a
// minimal number of cdr.download calls — see BatchGetMOS's doc comment.
//
// CDR timestamps are compared as raw Unix epoch values (timezone-agnostic
// — the "Date/Time" field in cdr.download's response), but the *query
// window itself* (start/starttime/end/endtime) is interpreted in the
// PBXware server's own local civil time, which this client has no
// reliable way to know in advance (confirmed empirically 2026-09-23: the
// API's own "timezone" parameter, when supplied, changes how the query
// window is interpreted, not the stored data, so it doesn't remove the
// need to already know the server's zone). Rather than guess an offset,
// this queries every calendar day the windows could plausibly fall on —
// today, yesterday, and tomorrow relative to each window's start, by
// SwarmDialer's own clock — wide enough to contain every call under any
// real timezone offset, and does the actual matching using the returned
// Unix timestamps, which need no timezone knowledge at all.
func (c *Client) batchFindCDRUniqueIDs(serverID int, windows []CallWindow) map[int]string {
	extSet := make(map[string]bool)
	daySet := make(map[string]time.Time)
	for _, w := range windows {
		extSet[w.Ext] = true
		for _, d := range []time.Time{w.Start, w.Start.Add(-24 * time.Hour), w.Start.Add(24 * time.Hour)} {
			daySet[d.Format("Jan-02-2006")] = d
		}
	}
	exts := make([]string, 0, len(extSet))
	for e := range extSet {
		exts = append(exts, e)
	}
	extFilter := strings.Join(exts, ",")

	var entries []cdrEntry
	for dateStr := range daySet {
		dayEntries, err := c.downloadCDRsForDay(serverID, extFilter, dateStr)
		if err != nil {
			continue // best-effort — a failed day just yields fewer matches, not a hard error
		}
		entries = append(entries, dayEntries...)
	}

	// Index by ext for fast matching; within an ext, pick the entry
	// closest to each window's start rather than the first one found —
	// at load-test call rates, more than one call from the same
	// extension can easily land inside the match tolerance (caught live
	// 2026-09-23: with tolerance loose enough, a neighboring call's CDR
	// was matched instead of the right one).
	byExt := make(map[string][]cdrEntry)
	for _, e := range entries {
		byExt[e.from] = append(byExt[e.from], e)
	}

	const tolerance = 15 * time.Second
	result := make(map[int]string, len(windows))
	for i, w := range windows {
		windowStart := w.Start.Add(-tolerance).Unix()
		windowEnd := w.End.Add(tolerance).Unix()
		target := w.Start.Unix()

		var bestID string
		var bestDiff int64
		haveBest := false
		for _, e := range byExt[w.Ext] {
			if e.startUnix < windowStart || e.startUnix > windowEnd {
				continue
			}
			diff := e.startUnix - target
			if diff < 0 {
				diff = -diff
			}
			if !haveBest || diff < bestDiff {
				bestID, bestDiff, haveBest = e.uniqueID, diff, true
			}
		}
		if haveBest {
			result[i] = bestID
		}
	}
	return result
}

// downloadCDRsForDay fetches every CDR for extFilter (one or more
// extensions, comma-separated) on one calendar day, handling pagination.
func (c *Client) downloadCDRsForDay(serverID int, extFilter, dateStr string) ([]cdrEntry, error) {
	var entries []cdrEntry
	page := 1
	for {
		params := url.Values{
			"start":     {dateStr},
			"starttime": {"00:00:00"},
			"end":       {dateStr},
			"endtime":   {"23:59:59"},
			"ext":       {extFilter},
			"limit":     {"1000"},
			"page":      {strconv.Itoa(page)},
		}
		if serverID != 0 {
			params.Set("server", strconv.Itoa(serverID))
		}
		body, err := c.call("pbxware.cdr.download", params)
		if err != nil {
			return nil, fmt.Errorf("downloading CDRs for %s: %w", dateStr, err)
		}
		rows, _ := body["csv"].([]any)
		for _, r := range rows {
			row, ok := r.([]any)
			if !ok || len(row) < 8 {
				continue
			}
			if entry, ok := parseCDRRow(row); ok {
				entries = append(entries, entry)
			}
		}
		nextPage, _ := body["next_page"].(bool)
		if !nextPage {
			break
		}
		page++
	}
	return entries, nil
}

// parseCDRRow extracts the From extension, start Unix timestamp, and
// Unique ID from one CDR CSV row. Multi-Tenant editions prepend an extra
// leading "Tenant" column, so fields are located from the back (the
// Unique ID always looks like "<unix>.<n>" and directly follows Status)
// rather than by a fixed index, to work on both editions without caring
// which one this is.
func parseCDRRow(row []any) (cdrEntry, bool) {
	str := func(v any) string { s, _ := v.(string); return s }

	uniqueIdx := -1
	for i, v := range row {
		s := str(v)
		if !strings.Contains(s, ".") {
			continue
		}
		if _, err := strconv.ParseInt(strings.SplitN(s, ".", 2)[0], 10, 64); err == nil {
			uniqueIdx = i
			break
		}
	}
	if uniqueIdx < 5 {
		return cdrEntry{}, false
	}
	// Working back from Unique ID: Status, Rating Cost, Rating Duration,
	// Total Duration, Date/Time — a fixed 5-column gap regardless of
	// whether the leading Tenant column is present, since that only
	// shifts every column uniformly.
	dateIdx := uniqueIdx - 5
	fromIdx := 0
	if uniqueIdx >= 13 {
		fromIdx = 1 // Multi-Tenant: Tenant is column 0, From is column 1
	}
	startUnix, _ := strconv.ParseInt(str(row[dateIdx]), 10, 64)
	from := str(row[fromIdx])
	// "From" can render as a bare extension ("1000") or a display name
	// with the extension in parens ("SwarmDialer 1000 (1000)") depending
	// on whether the request included a "server" param — normalize to
	// just the digits so it matches CallWindow.Ext either way.
	if idx := strings.LastIndex(from, "("); idx != -1 && strings.HasSuffix(from, ")") {
		from = from[idx+1 : len(from)-1]
	}

	return cdrEntry{from: from, startUnix: startUnix, uniqueID: str(row[uniqueIdx])}, true
}

// getMOSByUniqueID queries pbxware.cdr.mos for one call.
//
// A call's CDR shares its "Linked ID" with every channel leg involved
// (confirmed live 2026-09-23: a plain 2-party call returned 4 entries),
// and not every leg carries real RTP, so not every entry has real MOS
// data even when the call as a whole clearly does. This prefers the
// entry whose own "uniqueid" matches the CDR just looked up (the specific
// channel that CDR row refers to) and falls back to the first entry with
// any non-zero data — blindly trusting the first entry in the list was
// the original bug here, since with multiple channel legs that's not
// reliably the one with the real measurement.
func (c *Client) getMOSByUniqueID(uniqueID string) (mos CallMOS, ok bool, err error) {
	body, err := c.call("pbxware.cdr.mos", url.Values{"uniqueid": {uniqueID}})
	if err != nil {
		return CallMOS{}, false, err
	}
	data, _ := body["data"].([]any)

	var exact, firstNonZero *CallMOS
	for _, raw := range data {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		max, _ := toFloat(entry["maxmes"])
		avg, _ := toFloat(entry["avgmes"])
		min, _ := toFloat(entry["minmes"])
		m := CallMOS{Max: max, Avg: avg, Min: min}
		isZero := max == 0 && avg == 0 && min == 0

		if id, _ := entry["uniqueid"].(string); id == uniqueID {
			exact = &m
		}
		if !isZero && firstNonZero == nil {
			firstNonZero = &m
		}
	}
	if exact != nil && !(exact.Max == 0 && exact.Avg == 0 && exact.Min == 0) {
		return *exact, true, nil
	}
	if firstNonZero != nil {
		return *firstNonZero, true, nil
	}
	return CallMOS{}, false, nil // no channel leg for this call has MOS data yet
}

func toFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case string:
		var f float64
		_, err := fmt.Sscanf(t, "%f", &f)
		return f, err
	default:
		return 0, fmt.Errorf("unexpected type %T for float value", v)
	}
}
