package sfu

import (
	"log/slog"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type peer struct {
	id      string
	pc      *webrtc.PeerConnection
	session *session
	done    chan struct{}
	once    sync.Once

	negMu         sync.Mutex
	ready         bool
	dirty         bool
	subscriptions map[string]*webrtc.RTPSender
}

func (p *peer) sendMessage(typ string, data any) {
	p.session.sendMessage(typ, data)
}

func (p *peer) close() bool {
	closed := false
	p.once.Do(func() {
		closed = true
		close(p.done)
		p.pc.Close()
	})
	return closed
}

func (p *peer) acceptOffer(offer webrtc.SessionDescription) error {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return err
	}
	// An answer cannot add media sections absent from the client's offer.
	// All subscriptions are negotiated in a server offer after joined.
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return err
	}
	return nil
}

func (p *peer) activate(r *room) error {
	p.negMu.Lock()
	defer p.negMu.Unlock()

	r.mu.RLock()
	publications := make([]*publication, 0, len(r.publications))
	for _, pub := range r.publications {
		if pub.owner != p {
			publications = append(publications, pub)
		}
	}
	r.mu.RUnlock()

	for _, pub := range publications {
		if _, exists := p.subscriptions[pub.id]; exists {
			continue
		}
		if err := p.addSubscriptionLocked(r, pub); err != nil {
			return err
		}
		p.dirty = true
	}
	p.ready = true
	return p.sendOfferLocked()
}

func (p *peer) acceptAnswer(answer webrtc.SessionDescription) error {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return err
	}
	return p.sendOfferLocked()
}

func (p *peer) addSubscriptionLocked(r *room, pub *publication) error {
	if _, exists := p.subscriptions[pub.id]; exists {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.publications[pub.id] != pub {
		return nil
	}
	transceiver, err := p.pc.AddTransceiverFromTrack(pub.local, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		return err
	}
	sender := transceiver.Sender()
	p.subscriptions[pub.id] = sender
	go func() {
		for {
			packets, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, packet := range packets {
				switch feedback := packet.(type) {
				case *rtcp.PictureLossIndication:
					pub.requestKeyframe(feedback.SenderSSRC)
				case *rtcp.FullIntraRequest:
					pub.requestKeyframe(feedback.SenderSSRC)
				}
			}
		}
	}()
	return nil
}

func (p *peer) addSubscription(r *room, pub *publication) {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if !p.ready {
		p.dirty = true
		return
	}
	if _, exists := p.subscriptions[pub.id]; exists {
		return
	}
	if err := p.addSubscriptionLocked(r, pub); err != nil {
		slog.Warn("sfu add subscription failed", "peer", p.id, "error", err)
		return
	}
	p.dirty = true
	if err := p.sendOfferLocked(); err != nil {
		slog.Warn("sfu renegotiation failed", "peer", p.id, "error", err)
	}
}

func (p *peer) removeSubscription(id string) {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	sender := p.subscriptions[id]
	if sender == nil {
		return
	}
	delete(p.subscriptions, id)
	if err := p.pc.RemoveTrack(sender); err != nil {
		return
	}
	p.dirty = true
	if err := p.sendOfferLocked(); err != nil {
		slog.Warn("sfu renegotiation failed", "peer", p.id, "error", err)
	}
}

func (p *peer) sendOfferLocked() error {
	if !p.ready || !p.dirty || p.pc.SignalingState() != webrtc.SignalingStateStable {
		return nil
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	p.dirty = false
	p.sendMessage("offer", p.pc.LocalDescription())
	return nil
}
