package sipua

import (
	"context"
	"log/slog"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// The SIP library logs every UDP datagram it can't parse as SIP, with its
// contents. SwarmDialer's SIP ports also receive other LAN broadcasts
// (e.g. Ubiquiti device discovery), which flooded the service log as
// binary "failed to parse" errors. Those are dropped; parse failures of
// anything that looks like SIP are still logged.
func init() {
	sip.SetDefaultLogger(slog.New(nonSIPFilter{slog.Default().Handler()}))
}

type nonSIPFilter struct{ slog.Handler }

func (h nonSIPFilter) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "failed to parse" {
		data := ""
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "data" {
				data = a.Value.String()
				return false
			}
			return true
		})
		if !looksLikeSIP(data) {
			return nil
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h nonSIPFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return nonSIPFilter{h.Handler.WithAttrs(attrs)}
}

func (h nonSIPFilter) WithGroup(name string) slog.Handler {
	return nonSIPFilter{h.Handler.WithGroup(name)}
}

// looksLikeSIP reports whether data starts like a SIP request or response
// line (a method token followed by a space, or "SIP/2.0").
func looksLikeSIP(data string) bool {
	if strings.HasPrefix(data, "SIP/2.0") {
		return true
	}
	method, _, ok := strings.Cut(data, " ")
	if !ok || method == "" || len(method) > 16 {
		return false
	}
	for _, c := range method {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}
