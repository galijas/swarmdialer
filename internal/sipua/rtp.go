package sipua

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	rtpPacketDurationMs = 20

	// ntpEpochOffset is the number of seconds between the NTP epoch
	// (1900-01-01) and the Unix epoch (1970-01-01) — needed to build the
	// NTP timestamps RTCP Sender Reports carry.
	ntpEpochOffset = 2208988800

	// rtcpReportInterval is how often we send RTCP SR/RR. The RFC 3550
	// convention is ~5s, but SwarmDialer's test calls are often much
	// shorter than that — a longer interval risks a call ending before a
	// single report goes out, which is exactly the "no MOS data" failure
	// mode this whole feature exists to fix. Confirmed via a live PBXware
	// test call.
	rtcpReportInterval = 2 * time.Second
)

// RTPSession sends and receives one audio RTP stream — real packets with
// correct sequence numbers/timestamps, carrying silence payload in
// whichever codec was selected — for one call leg, and exchanges RTCP
// Sender/Receiver Reports alongside it so PBXware has real jitter/loss
// data to compute a MOS score from (see docs/PROJECT_STATE.md's 2026-09-23
// finding: without RTCP, PBXware's CDR MOS is always 0 — it has nothing to
// compute from — but it can compute a real score purely from what it
// itself observes about our incoming RTP/RTCP, without our peer needing to
// do anything special in return).
type RTPSession struct {
	conn     *net.UDPConn // RTP socket
	rtcpConn *net.UDPConn // RTCP socket — RTP port + 1, the standard convention (RFC 3605) absent an explicit a=rtcp: SDP override

	remoteAddr     *net.UDPAddr
	rtcpRemoteAddr *net.UDPAddr

	codec Codec
	spec  codecSpec
	enc   frameEncoder

	ssrc        uint32
	seq         uint16
	timestamp   uint32
	packetsSent atomic.Uint64
	packetsRecv atomic.Uint64
	octetsSent  atomic.Uint64

	sessionStart time.Time

	stop     chan struct{}
	stopOnce sync.Once

	mu         sync.Mutex
	rtcpState  rtcpReceiveState
	lastSR     rtcp.SenderReport // last SR we ourselves sent, kept for logging/debugging only
	haveLastSR bool
}

// rtcpReceiveState is what we need to build a ReceptionReport (RFC 3550
// §6.4.1) about the stream we're receiving from our peer.
type rtcpReceiveState struct {
	havePeerSSRC bool
	peerSSRC     uint32

	haveBase   bool
	baseSeq    uint16 // first sequence number seen — extended-sequence tracking is relative to this
	highestSeq uint32 // extended (cycles<<16 | seq16), monotonically non-decreasing
	cycles     uint16

	packetsReceived uint32

	// RFC 3550 Appendix A.8 jitter estimator state.
	haveLastTransit bool
	lastTransit     int64
	jitter          float64 // running estimate, in the codec's clock-rate units

	// For DLSR (delay since last SR) — set when we receive an SR from the peer.
	haveLastPeerSR bool
	lastPeerSRNTP  uint64
	lastPeerSRRecv time.Time
}

// NewRTPSession opens RTP and RTCP UDP sockets for the given codec. Passing
// localPort 0 lets the OS assign a free ephemeral RTP port (use LocalPort to
// read it back) — RTCP always binds to that port + 1.
//
// When localPort is 0, the RTP port itself is picked by the OS with no way
// to reserve port+1 for RTCP atomically alongside it — under concurrent
// call setup (many sessions opening sockets at once), another session can
// grab that +1 port first, so the second bind is retried with a fresh OS-
// assigned RTP port rather than failing the call outright (confirmed live
// 2026-09-23: "address already in use" on the RTCP bind during a 25-call
// batch). An explicit non-zero localPort is a caller's deliberate choice,
// so it's tried exactly once — retrying the same port would just fail the
// same way again.
func NewRTPSession(localPort int, codec Codec) (*RTPSession, error) {
	maxAttempts := 10
	if localPort != 0 {
		maxAttempts = 1
	}

	var conn, rtcpConn *net.UDPConn
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		conn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: localPort})
		if err != nil {
			return nil, fmt.Errorf("opening RTP socket on port %d: %w", localPort, err)
		}
		rtpPort := conn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: rtpPort + 1})
		if err == nil {
			break
		}
		conn.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("opening RTCP socket: %w", err)
	}

	spec := codec.spec()
	return &RTPSession{
		conn:         conn,
		rtcpConn:     rtcpConn,
		codec:        codec,
		spec:         spec,
		enc:          spec.newEncoder(),
		ssrc:         rand.Uint32(),
		seq:          uint16(rand.Uint32()),
		timestamp:    rand.Uint32(),
		sessionStart: time.Now(),
		stop:         make(chan struct{}),
	}, nil
}

// LocalPort is the UDP port this session's RTP is bound to — put this in
// the SDP media line we offer/answer with (RTCP is always this + 1).
func (s *RTPSession) LocalPort() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// SetRemote sets the peer's RTP address, learned from their SDP
// offer/answer. RTCP goes to the same host, port + 1.
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("resolving remote RTP address %s:%d: %w", ip, port, err)
	}
	s.remoteAddr = addr
	rtcpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port+1))
	if err != nil {
		return fmt.Errorf("resolving remote RTCP address %s:%d: %w", ip, port+1, err)
	}
	s.rtcpRemoteAddr = rtcpAddr
	return nil
}

// Start begins sending a silence packet every 20ms, receiving/counting
// incoming packets, and exchanging RTCP SR/RR, until ctx is canceled or
// Stop is called.
func (s *RTPSession) Start(ctx context.Context) {
	go s.sendLoop(ctx)
	go s.recvLoop(ctx)
	go s.rtcpLoop(ctx)
	go s.rtcpRecvLoop(ctx)
}

// Stop halts sending/receiving and releases both sockets. Safe to call more
// than once (or concurrently) on the same session — see the doc comment
// this had before RTCP support was added; the same "two goroutines racing
// to end the same call" scenario applies equally to the RTCP sockets.
func (s *RTPSession) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.conn.Close()
		s.rtcpConn.Close()
	})
}

// Stats returns packet counts sent/received so far — useful for confirming
// media is actually flowing, and for the live status dashboard.
func (s *RTPSession) Stats() (sent, recv uint64) {
	return s.packetsSent.Load(), s.packetsRecv.Load()
}

func (s *RTPSession) sendLoop(ctx context.Context) {
	ticker := time.NewTicker(rtpPacketDurationMs * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			if s.remoteAddr == nil {
				continue // haven't learned the peer's address yet
			}
			payload := s.enc.EncodeSilenceFrame()
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    s.spec.payloadType,
					SequenceNumber: s.seq,
					Timestamp:      s.timestamp,
					SSRC:           s.ssrc,
				},
				Payload: payload,
			}
			s.seq++
			s.timestamp += uint32(s.spec.samplesPerPacket)

			buf, err := pkt.Marshal()
			if err != nil {
				continue
			}
			if _, err := s.conn.WriteToUDP(buf, s.remoteAddr); err == nil {
				s.packetsSent.Add(1)
				s.octetsSent.Add(uint64(len(payload)))
			}
		}
	}
}

func (s *RTPSession) recvLoop(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		default:
		}
		if err := s.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			return
		}
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			continue // timeout (checked against ctx/stop above) or closed
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err == nil {
			s.packetsRecv.Add(1)
			s.onRTPReceived(&pkt)
		}
	}
}

// onRTPReceived updates the RFC 3550 receive-side bookkeeping (extended
// sequence tracking for loss, and the Appendix A.8 jitter estimator) that
// our own outgoing RTCP Receiver Reports are built from.
func (s *RTPSession) onRTPReceived(pkt *rtp.Packet) {
	now := time.Now()
	// "Arrival timestamp" expressed in the codec's own clock-rate units,
	// relative to session start — only used as differences (in the jitter
	// calc below), so the arbitrary reference point doesn't matter.
	arrival := uint32(now.Sub(s.sessionStart).Seconds() * float64(s.spec.clockRateForSDP))

	s.mu.Lock()
	defer s.mu.Unlock()

	st := &s.rtcpState
	if !st.havePeerSSRC {
		st.havePeerSSRC = true
		st.peerSSRC = pkt.SSRC
	}
	if !st.haveBase {
		st.haveBase = true
		st.baseSeq = pkt.SequenceNumber
		st.highestSeq = uint32(pkt.SequenceNumber)
	} else {
		prev16 := uint16(st.highestSeq)
		if pkt.SequenceNumber < prev16 && prev16-pkt.SequenceNumber > 0x8000 {
			// Wrapped around 65536 — a genuinely later packet with a
			// smaller 16-bit number.
			st.cycles++
		}
		ext := uint32(st.cycles)<<16 | uint32(pkt.SequenceNumber)
		if ext > st.highestSeq {
			st.highestSeq = ext
		}
	}
	st.packetsReceived++

	transit := int64(arrival) - int64(pkt.Timestamp)
	if st.haveLastTransit {
		d := transit - st.lastTransit
		if d < 0 {
			d = -d
		}
		st.jitter += (float64(d) - st.jitter) / 16
	}
	st.lastTransit = transit
	st.haveLastTransit = true
}

func (s *RTPSession) rtcpRecvLoop(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		default:
		}
		if err := s.rtcpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			return
		}
		n, _, err := s.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, p := range packets {
			sr, ok := p.(*rtcp.SenderReport)
			if !ok {
				continue
			}
			s.mu.Lock()
			s.rtcpState.haveLastPeerSR = true
			s.rtcpState.lastPeerSRNTP = sr.NTPTime
			s.rtcpState.lastPeerSRRecv = time.Now()
			s.mu.Unlock()
		}
	}
}

func (s *RTPSession) rtcpLoop(ctx context.Context) {
	ticker := time.NewTicker(rtcpReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.sendRTCPReport()
		}
	}
}

func (s *RTPSession) sendRTCPReport() {
	if s.rtcpRemoteAddr == nil {
		return
	}

	now := time.Now()
	sr := rtcp.SenderReport{
		SSRC:        s.ssrc,
		NTPTime:     toNTPTime(now),
		RTPTime:     s.timestamp,
		PacketCount: uint32(s.packetsSent.Load()),
		OctetCount:  uint32(s.octetsSent.Load()),
	}

	packets := []rtcp.Packet{&sr}

	s.mu.Lock()
	st := s.rtcpState
	s.lastSR = sr
	s.haveLastSR = true
	s.mu.Unlock()

	if st.havePeerSSRC && st.haveBase {
		expected := st.highestSeq - uint32(st.baseSeq) + 1
		var fractionLost uint8
		var totalLost uint32
		if expected >= st.packetsReceived {
			totalLost = expected - st.packetsReceived
			if expected > 0 {
				fractionLost = uint8((totalLost * 256) / expected)
			}
		}

		var lsr, dlsr uint32
		if st.haveLastPeerSR {
			lsr = uint32(st.lastPeerSRNTP >> 16) // middle 32 bits of the peer's NTP timestamp
			delay := now.Sub(st.lastPeerSRRecv)
			dlsr = uint32(delay.Seconds() * 65536)
		}

		rr := rtcp.ReceiverReport{
			SSRC: s.ssrc,
			Reports: []rtcp.ReceptionReport{{
				SSRC:               st.peerSSRC,
				FractionLost:       fractionLost,
				TotalLost:          totalLost,
				LastSequenceNumber: st.highestSeq,
				Jitter:             uint32(math.Round(st.jitter)),
				LastSenderReport:   lsr,
				Delay:              dlsr,
			}},
		}
		packets = append(packets, &rr)
	}

	buf, err := rtcp.Marshal(packets)
	if err != nil {
		return
	}
	_, _ = s.rtcpConn.WriteToUDP(buf, s.rtcpRemoteAddr)
}

// toNTPTime converts a time.Time to the 64-bit fixed-point NTP timestamp
// format RTCP Sender Reports use (32 bits of seconds since the NTP epoch,
// 32 bits of fractional seconds).
func toNTPTime(t time.Time) uint64 {
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(float64(t.Nanosecond()) * (1 << 32) / 1e9)
	return secs<<32 | frac
}
