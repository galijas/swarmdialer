package sipua

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// dialogSession is the subset of *sipgo.DialogClientSession and
// *sipgo.DialogServerSession that Call needs — just enough to hang up.
type dialogSession interface {
	Bye(ctx context.Context) error
}

// Call bundles one established call's SIP dialog and its RTP media session,
// so both get torn down together.
type Call struct {
	session dialogSession
	RTP     *RTPSession

	phone  *Phone
	callID string
}

// Hangup sends BYE and stops the RTP stream together.
func (c *Call) Hangup(ctx context.Context) error {
	c.RTP.Stop()
	c.phone.calls.Delete(c.callID)
	return c.session.Bye(ctx)
}

// Phone is one simulated PBXware extension: it can register, place outbound
// calls, and auto-answer inbound calls, all bound to one local UDP port (its
// own Contact address). Each concurrently-simulated extension in a load test
// needs its own Phone on its own port, the same way a real softphone would.
type Phone struct {
	UA           *sipgo.UserAgent
	Client       *sipgo.Client
	Server       *sipgo.Server
	DialogClient *sipgo.DialogClientCache
	DialogServer *sipgo.DialogServerCache
	Endpoint     Endpoint
	LocalIP      string
	LocalPort    int

	// calls maps Call-ID -> *RTPSession for calls currently active on this
	// phone, so the OnBye handler (which only sees a *sip.Request, not our
	// Call struct) can stop the right RTP stream when the other party hangs
	// up. Keyed by plain Call-ID rather than sipgo's computed dialog ID to
	// stay independent of UAC/UAS tag-ordering details.
	calls sync.Map
}

// NewPhone sets up a Phone listening on localIP:localPort and starts serving
// in the background (until ctx is canceled). Call AutoAnswer afterward if
// this phone should accept inbound calls.
func NewPhone(ctx context.Context, localIP string, localPort int, ep Endpoint) (*Phone, error) {
	ua, err := sipgo.NewUA()
	if err != nil {
		return nil, fmt.Errorf("creating user agent: %w", err)
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("creating server: %w", err)
	}

	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname(localIP), sipgo.WithClientPort(localPort))
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}

	contactHDR := sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: ep.Username, Host: localIP, Port: localPort},
	}
	dialogClient := sipgo.NewDialogClientCache(client, contactHDR)
	dialogServer := sipgo.NewDialogServerCache(client, contactHDR)

	p := &Phone{
		UA:           ua,
		Client:       client,
		Server:       server,
		DialogClient: dialogClient,
		DialogServer: dialogServer,
		Endpoint:     ep,
		LocalIP:      localIP,
		LocalPort:    localPort,
	}

	server.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		// PBXware pings registered contacts with OPTIONS (the extension's
		// "qualify" setting) to check reachability. Answering keeps us
		// looking healthy — at load-test scale (hundreds of registered
		// phones), leaving this unanswered risks PBXware marking contacts
		// unreachable.
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			log.Printf("sipua: %s: responding to OPTIONS: %v", ep.AOR, err)
		}
	})
	server.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := dialogServer.ReadAck(req, tx); err != nil {
			log.Printf("sipua: %s: ACK for unknown dialog: %v", ep.AOR, err)
		}
	})
	server.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		// This phone may be either side of the dialog being torn down
		// (it placed the call, or it received it) — try both caches.
		if err := dialogServer.ReadBye(req, tx); err != nil {
			if err := dialogClient.ReadBye(req, tx); err != nil {
				log.Printf("sipua: %s: BYE for unknown dialog: %v", ep.AOR, err)
				// Respond explicitly rather than staying silent — silence
				// makes PBXware retransmit the BYE repeatedly (observed
				// during rapid dev-loop testing that reused fixed ports
				// across short-lived processes; not seen in normal
				// long-running operation, but responding is correct
				// regardless of why a dialog didn't match).
				_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
			}
		}
		// Stop the RTP stream for this call regardless of which cache
		// matched — the other party hanging up ends our media too.
		if callID := req.CallID(); callID != nil {
			if v, ok := p.calls.LoadAndDelete(callID.Value()); ok {
				v.(*RTPSession).Stop()
			}
		}
	})

	go func() {
		addr := fmt.Sprintf("0.0.0.0:%d", localPort)
		if err := server.ListenAndServe(ctx, "udp", addr); err != nil && ctx.Err() == nil {
			log.Printf("sipua: %s: listener on %s stopped: %v", ep.AOR, addr, err)
		}
	}()

	return p, nil
}

// Close shuts down the phone's client and server.
func (p *Phone) Close() {
	p.Client.Close()
	p.Server.Close()
	p.UA.Close()
}

// Register performs SIP REGISTER for this phone against PBXware. The Contact
// we advertise includes our actual listening port, so PBXware can deliver
// inbound INVITEs back to this phone (not just to the default SIP port).
func (p *Phone) Register(ctx context.Context, dialDestination, sipDomain string, expirySeconds int) (int, error) {
	localContact := fmt.Sprintf("%s:%d", p.LocalIP, p.LocalPort)
	return Register(ctx, p.Client, dialDestination, sipDomain, localContact, p.Endpoint, expirySeconds)
}

// AutoAnswer makes this phone accept every inbound INVITE: answer with an
// SDP offering G.711, learn the caller's RTP address from their offer, and
// start exchanging silence-payload RTP for the call's duration.
func (p *Phone) AutoAnswer(ctx context.Context) {
	p.Server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dlg, err := p.DialogServer.ReadInvite(req, tx)
		if err != nil {
			log.Printf("sipua: %s: failed to read invite: %v", p.Endpoint.AOR, err)
			return
		}
		if err := dlg.Respond(sip.StatusTrying, "Trying", nil); err != nil {
			log.Printf("sipua: %s: responding 100: %v", p.Endpoint.AOR, err)
			return
		}
		if err := dlg.Respond(sip.StatusRinging, "Ringing", nil); err != nil {
			log.Printf("sipua: %s: responding 180: %v", p.Endpoint.AOR, err)
			return
		}

		remoteIP, remotePort, err := parseSDPMedia(req.Body())
		if err != nil {
			log.Printf("sipua: %s: parsing caller's SDP offer: %v", p.Endpoint.AOR, err)
			return
		}

		rtpSession, err := NewRTPSession(0)
		if err != nil {
			log.Printf("sipua: %s: allocating RTP session: %v", p.Endpoint.AOR, err)
			return
		}
		if err := rtpSession.SetRemote(remoteIP, remotePort); err != nil {
			log.Printf("sipua: %s: setting RTP remote: %v", p.Endpoint.AOR, err)
			rtpSession.Stop()
			return
		}

		sdp := buildSDP(p.LocalIP, rtpSession.LocalPort())
		if err := dlg.RespondSDP(sdp); err != nil {
			log.Printf("sipua: %s: responding 200 with SDP: %v", p.Endpoint.AOR, err)
			rtpSession.Stop()
			return
		}

		if callID := req.CallID(); callID != nil {
			p.calls.Store(callID.Value(), rtpSession)
		}
		rtpSession.Start(ctx)

		log.Printf("sipua: %s: answered call from %s, RTP peer %s:%d", p.Endpoint.AOR, req.From().Address.User, remoteIP, remotePort)
	})
}

// Dial places a call from this phone to calleeAOR (an extension number),
// waits for it to be answered, and starts exchanging silence-payload RTP
// with the answering party's advertised address (learned from their SDP
// answer). dialDestination is the actual network target (PBXware's
// host:port); sipDomain is the domain to put in the SIP headers (see
// Register's doc comment for why these can differ).
func (p *Phone) Dial(ctx context.Context, dialDestination, sipDomain, calleeAOR string) (*Call, error) {
	recipient := sip.Uri{}
	if err := sip.ParseUri(fmt.Sprintf("sip:%s@%s", calleeAOR, sipDomain), &recipient); err != nil {
		return nil, fmt.Errorf("parsing recipient URI: %w", err)
	}

	rtpSession, err := NewRTPSession(0)
	if err != nil {
		return nil, fmt.Errorf("allocating RTP session: %w", err)
	}

	req := sip.NewRequest(sip.INVITE, recipient)
	req.SetDestination(dialDestination)
	req.SetBody(buildSDP(p.LocalIP, rtpSession.LocalPort()))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	// Same identity fix as Register: without an explicit From, sipgo
	// defaults to the UA's generic name, which PBXware can't match to a
	// real endpoint.
	fromIdentity := sip.Uri{Scheme: "sip", User: p.Endpoint.Username, Host: recipient.Host, Port: recipient.Port}
	from := sip.FromHeader{DisplayName: p.Endpoint.Username, Address: fromIdentity}
	from.Params = sip.NewParams()
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(&from)
	req.AppendHeader(&sip.ToHeader{DisplayName: calleeAOR, Address: recipient})

	sess, err := p.DialogClient.WriteInvite(ctx, req)
	if err != nil {
		rtpSession.Stop()
		return nil, fmt.Errorf("sending invite: %w", err)
	}

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: p.Endpoint.Username,
		Password: p.Endpoint.Password,
	}); err != nil {
		rtpSession.Stop()
		return nil, fmt.Errorf("waiting for answer: %w", err)
	}

	if err := sess.Ack(ctx); err != nil {
		rtpSession.Stop()
		return nil, fmt.Errorf("sending ack: %w", err)
	}

	remoteIP, remotePort, err := parseSDPMedia(sess.InviteResponse.Body())
	if err != nil {
		rtpSession.Stop()
		return nil, fmt.Errorf("parsing callee's SDP answer: %w", err)
	}
	if err := rtpSession.SetRemote(remoteIP, remotePort); err != nil {
		rtpSession.Stop()
		return nil, fmt.Errorf("setting RTP remote: %w", err)
	}
	rtpSession.Start(ctx)

	callID := req.CallID().Value()
	p.calls.Store(callID, rtpSession)

	return &Call{session: sess, RTP: rtpSession, phone: p, callID: callID}, nil
}
