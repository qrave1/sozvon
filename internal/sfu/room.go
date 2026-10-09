package sfu

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type room struct {
	id           string
	mu           sync.RWMutex
	chatMu       sync.Mutex
	peers        map[string]*peer
	profiles     map[string]peerProfile
	announced    map[string]bool
	publications map[string]*publication
}

type publication struct {
	id     string
	owner  *peer
	remote *webrtc.TrackRemote
	local  *webrtc.TrackLocalStaticRTP
}

type peerProfile struct {
	Name        string `json:"name"`
	MicOn       bool   `json:"micOn"`
	CamOn       bool   `json:"camOn"`
	ScreenShare bool   `json:"screenShare"`
	Color       string `json:"color"`
}

func parseProfile(raw json.RawMessage) peerProfile {
	if len(raw) == 0 {
		return peerProfile{MicOn: true, CamOn: true}
	}
	var profile peerProfile
	if json.Unmarshal(raw, &profile) != nil {
		return peerProfile{}
	}
	name := []rune(strings.TrimSpace(profile.Name))
	if len(name) > 64 {
		name = name[:64]
	}
	profile.Name = string(name)
	if len(profile.Color) != 7 || profile.Color[0] != '#' {
		profile.Color = ""
	} else {
		for _, c := range profile.Color[1:] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				profile.Color = ""
				break
			}
		}
	}
	return profile
}

func (r *room) announce(p *peer, chat *chatStore) bool {
	r.chatMu.Lock()
	defer r.chatMu.Unlock()
	r.mu.Lock()
	if r.peers[p.id] != p {
		r.mu.Unlock()
		return false
	}
	peers := make([]*peer, 0, len(r.peers))
	ids := make([]string, 0, len(r.peers))
	profiles := make(map[string]peerProfile)
	for _, other := range r.peers {
		if other != p && r.announced[other.id] {
			peers = append(peers, other)
			ids = append(ids, other.id)
			profiles[other.id] = r.profiles[other.id]
		}
	}
	r.announced[p.id] = true
	profile := r.profiles[p.id]
	history := chat.history(r.id, time.Now())
	p.sendMessage("joined", map[string]any{"id": p.id, "answer": p.pc.LocalDescription(), "peers": ids, "profiles": profiles, "chatHistory": history})
	r.mu.Unlock()
	for _, other := range peers {
		other.sendMessage("peer_joined", map[string]any{"id": p.id, "profile": profile})
	}
	return true
}

func (r *room) broadcastChat(store *chatStore, sender *peer, text string) {
	r.chatMu.Lock()
	defer r.chatMu.Unlock()
	r.mu.RLock()
	if r.peers[sender.id] != sender || !r.announced[sender.id] {
		r.mu.RUnlock()
		return
	}
	profile := r.profiles[sender.id]
	recipients := make([]*peer, 0, len(r.peers))
	for _, p := range r.peers {
		if r.announced[p.id] {
			recipients = append(recipients, p)
		}
	}
	r.mu.RUnlock()
	msg := store.add(r.id, profile, sender.id, text, time.Now())
	for _, p := range recipients {
		p.sendMessage("chat_message", msg)
	}
}

func (r *room) updateProfile(p *peer, profile peerProfile) {
	r.mu.Lock()
	if r.peers[p.id] != p {
		r.mu.Unlock()
		return
	}
	previous := r.profiles[p.id]
	refreshVideo := (profile.CamOn && !previous.CamOn) || profile.ScreenShare != previous.ScreenShare
	r.profiles[p.id] = profile
	peers := make([]*peer, 0, len(r.peers))
	if r.announced[p.id] {
		for _, other := range r.peers {
			if other != p && r.announced[other.id] {
				peers = append(peers, other)
			}
		}
	}
	r.mu.Unlock()
	for _, other := range peers {
		other.sendMessage("state", map[string]any{"id": p.id, "profile": profile})
	}
	if refreshVideo {
		r.requestPublishedKeyframes(p)
	}
}

func (r *room) removePeer(p *peer) ([]*peer, []*publication, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peers[p.id] != p {
		return nil, nil, false
	}
	announced := r.announced[p.id]
	delete(r.peers, p.id)
	delete(r.profiles, p.id)
	delete(r.announced, p.id)
	peers := make([]*peer, 0, len(r.peers))
	if announced {
		for _, other := range r.peers {
			if r.announced[other.id] {
				peers = append(peers, other)
			}
		}
	}
	publications := make([]*publication, 0)
	for _, pub := range r.publications {
		if pub.owner == p {
			publications = append(publications, pub)
		}
	}
	return peers, publications, announced
}

func (r *room) requestPublishedKeyframes(p *peer) {
	r.mu.RLock()
	publications := make([]*publication, 0)
	for _, pub := range r.publications {
		if pub.owner == p {
			publications = append(publications, pub)
		}
	}
	r.mu.RUnlock()
	for _, pub := range publications {
		pub.requestKeyframe(0)
	}
}

func (r *room) requestSubscribedKeyframes(p *peer) {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	r.mu.RLock()
	publications := make([]*publication, 0, len(p.subscriptions))
	for id := range p.subscriptions {
		if pub := r.publications[id]; pub != nil && pub.remote.Kind() == webrtc.RTPCodecTypeVideo {
			publications = append(publications, pub)
		}
	}
	r.mu.RUnlock()
	for _, pub := range publications {
		pub.requestKeyframe(0)
	}
}

func (pub *publication) requestKeyframe(senderSSRC uint32) {
	if pub.remote.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	_ = pub.owner.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: senderSSRC, MediaSSRC: uint32(pub.remote.SSRC())},
	})
}

func (r *room) publish(p *peer, track *webrtc.TrackRemote) {
	local, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, track.ID(), p.id)
	if err != nil {
		slog.Warn("sfu publish failed", "peer", p.id, "error", err)
		return
	}
	pub := &publication{id: p.id + ":" + track.ID(), owner: p, remote: track, local: local}
	r.mu.Lock()
	if r.peers[p.id] != p {
		r.mu.Unlock()
		return
	}
	r.publications[pub.id] = pub
	peers := make([]*peer, 0, len(r.peers))
	for _, other := range r.peers {
		if other != p {
			peers = append(peers, other)
		}
	}
	r.mu.Unlock()
	for _, other := range peers {
		other.addSubscription(r, pub)
	}
	defer r.removePublication(pub)
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, webrtc.ErrConnectionClosed) {
				slog.Debug("sfu publication ended", "peer", p.id, "error", err)
			}
			return
		}
		// MID, RID and transport sequence extensions belong to the inbound
		// connection. Pion's outbound interceptors generate their own extensions.
		packet.Header.Extension = false
		packet.Header.ExtensionProfile = 0
		packet.Header.Extensions = nil
		if err := local.WriteRTP(packet); err != nil {
			slog.Debug("sfu RTP write failed", "peer", p.id, "error", err)
		}
	}
}

func (r *room) removePublication(pub *publication) {
	r.mu.Lock()
	if r.publications[pub.id] != pub {
		r.mu.Unlock()
		return
	}
	delete(r.publications, pub.id)
	peers := make([]*peer, 0, len(r.peers))
	for _, p := range r.peers {
		if p != pub.owner {
			peers = append(peers, p)
		}
	}
	r.mu.Unlock()
	for _, p := range peers {
		p.removeSubscription(pub.id)
	}
}
