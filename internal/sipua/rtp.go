package sipua

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const (
	rtpClockRate        = 8000 // G.711, both ulaw and alaw
	rtpPacketDurationMs = 20
	samplesPerPacket    = rtpClockRate * rtpPacketDurationMs / 1000 // 160
	payloadTypePCMU     = 0
)

// silencePCMU is one packet's worth of PCMU (u-law) silence. 0xFF is the
// u-law encoding of zero amplitude.
var silencePCMU = func() []byte {
	b := make([]byte, samplesPerPacket)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}()

// RTPSession sends and receives one audio RTP stream — real packets with
// correct sequence numbers/timestamps, carrying silence payload — for one
// call leg. This is what actually simulates call load on PBXware, as
// opposed to just the SIP signaling.
type RTPSession struct {
	conn        *net.UDPConn
	remoteAddr  *net.UDPAddr
	ssrc        uint32
	seq         uint16
	timestamp   uint32
	packetsSent atomic.Uint64
	packetsRecv atomic.Uint64
	stop        chan struct{}
}

// NewRTPSession opens a UDP socket for RTP traffic. Passing localPort 0 lets
// the OS assign a free ephemeral port (use LocalPort to read it back).
func NewRTPSession(localPort int) (*RTPSession, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("opening RTP socket on port %d: %w", localPort, err)
	}
	return &RTPSession{
		conn:      conn,
		ssrc:      rand.Uint32(),
		seq:       uint16(rand.Uint32()),
		timestamp: rand.Uint32(),
		stop:      make(chan struct{}),
	}, nil
}

// LocalPort is the UDP port this session is bound to — put this in the SDP
// media line we offer/answer with.
func (s *RTPSession) LocalPort() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// SetRemote sets the peer's RTP address, learned from their SDP offer/answer.
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("resolving remote RTP address %s:%d: %w", ip, port, err)
	}
	s.remoteAddr = addr
	return nil
}

// Start begins sending a silence packet every 20ms and counting incoming
// packets, until ctx is canceled or Stop is called.
func (s *RTPSession) Start(ctx context.Context) {
	go s.sendLoop(ctx)
	go s.recvLoop(ctx)
}

// Stop halts sending/receiving and releases the socket. Safe to call at most
// once per session.
func (s *RTPSession) Stop() {
	close(s.stop)
	s.conn.Close()
}

// Stats returns packet counts sent/received so far — useful for confirming
// media is actually flowing, and later for the live status dashboard.
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
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    payloadTypePCMU,
					SequenceNumber: s.seq,
					Timestamp:      s.timestamp,
					SSRC:           s.ssrc,
				},
				Payload: silencePCMU,
			}
			s.seq++
			s.timestamp += samplesPerPacket

			buf, err := pkt.Marshal()
			if err != nil {
				continue
			}
			if _, err := s.conn.WriteToUDP(buf, s.remoteAddr); err == nil {
				s.packetsSent.Add(1)
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
		}
	}
}
