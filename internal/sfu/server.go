package sfu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 20
)

type message struct {
	Type    string          `json:"type"`
	Room    string          `json:"room,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Profile json.RawMessage `json:"profile,omitempty"`
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

type publication struct {
	id     string
	owner  *peer
	remote *webrtc.TrackRemote
	local  *webrtc.TrackLocalStaticRTP
}

type room struct {
	id           string
	mu           sync.RWMutex
	peers        map[string]*peer
	publications map[string]*publication
}

type Server struct {
	mu      sync.Mutex
	rooms   map[string]*room
	api     *webrtc.API
	udpConn net.PacketConn
}

func NewServer() *Server {
	return &Server{rooms: make(map[string]*room), api: webrtc.NewAPI()}
}

func NewServerWithUDP(port uint16, publicIP string) (*Server, error) {
	if port == 0 {
		return nil, errors.New("SFU UDP port is required")
	}
	if publicIP != "" && net.ParseIP(publicIP) == nil {
		return nil, fmt.Errorf("invalid SFU public IP %q", publicIP)
	}
	conn, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(port))))
	if err != nil {
		return nil, fmt.Errorf("listen SFU UDP: %w", err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetICEUDPMux(webrtc.NewICEUDPMux(nil, conn))
	if publicIP != "" {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{publicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteAppend,
		}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("configure SFU public IP: %w", err)
		}
	}
	return &Server{
		rooms:   make(map[string]*room),
		api:     webrtc.NewAPI(webrtc.WithSettingEngine(settings)),
		udpConn: conn,
	}, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	peers := make([]*peer, 0)
	for _, r := range s.rooms {
		r.mu.RLock()
		for _, p := range r.peers {
			peers = append(peers, p)
		}
		r.mu.RUnlock()
	}
	s.mu.Unlock()
	for _, p := range peers {
		p.close(s)
	}
	if s.udpConn != nil {
		return s.udpConn.Close()
	}
	return nil
}

func (s *Server) addPeer(id string, p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[id]
	if r == nil {
		r = &room{id: id, peers: make(map[string]*peer), publications: make(map[string]*publication)}
		s.rooms[id] = r
	}
	r.mu.Lock()
	p.room = r
	r.peers[p.id] = p
	r.mu.Unlock()
}

func (s *Server) removeEmptyRoom(r *room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.peers) == 0 && s.rooms[r.id] == r {
		delete(s.rooms, r.id)
	}
}

type peer struct {
	id      string
	room    *room
	profile peerProfile
	pc      *webrtc.PeerConnection
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	send    chan message
	done    chan struct{}
	once    sync.Once

	negMu         sync.Mutex
	ready         bool
	announced     bool
	dirty         bool
	subscriptions map[string]*webrtc.RTPSender
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessageSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var first message
	joinCtx, joinCancel := context.WithTimeout(ctx, pongWait)
	err = wsjson.Read(joinCtx, ws, &first)
	joinCancel()
	if err != nil || first.Type != "join" || first.Room == "" || len(first.Room) > 128 {
		ws.CloseNow()
		return
	}

	var offer webrtc.SessionDescription
	if err := json.Unmarshal(first.Data, &offer); err != nil || offer.Type != webrtc.SDPTypeOffer {
		ws.CloseNow()
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		slog.Error("sfu peer creation failed", "error", err)
		ws.CloseNow()
		return
	}

	p := &peer{
		id: uuid.NewString(), pc: pc, ws: ws, ctx: ctx, cancel: cancel,
		profile: parseProfile(first.Profile),
		send:    make(chan message, 256), done: make(chan struct{}),
		subscriptions: make(map[string]*webrtc.RTPSender),
	}
	s.addPeer(first.Room, p)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		p.publish(track)
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			p.sendMessage("candidate", candidate.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed {
			slog.Warn("sfu peer connection failed", "peer", p.id, "room", p.room.id)
			go p.close(s)
		} else if state == webrtc.PeerConnectionStateClosed {
			go p.close(s)
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Info("sfu ICE state changed", "peer", p.id, "room", p.room.id, "state", state.String())
	})

	go p.writeLoop()
	if err := p.acceptOffer(offer); err != nil {
		slog.Warn("sfu offer failed", "peer", p.id, "error", err)
		p.close(s)
		return
	}
	p.announce()

	for {
		var msg message
		if err := wsjson.Read(ctx, ws, &msg); err != nil {
			if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway && !errors.Is(err, context.Canceled) {
				slog.Info("sfu websocket read ended", "peer", p.id, "room", p.room.id, "error", err)
			}
			break
		}
		switch msg.Type {
		case "answer":
			var answer webrtc.SessionDescription
			if json.Unmarshal(msg.Data, &answer) == nil && answer.Type == webrtc.SDPTypeAnswer {
				if err := p.acceptAnswer(answer); err != nil {
					slog.Warn("sfu answer failed", "peer", p.id, "error", err)
					p.close(s)
					return
				}
			}
		case "candidate":
			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal(msg.Data, &candidate); err != nil || candidate.Candidate == "" {
				break
			}
			if err := p.pc.AddICECandidate(candidate); err != nil {
				slog.Warn("sfu ICE candidate rejected", "peer", p.id, "error", err)
			}
		case "state":
			p.updateProfile(parseProfile(msg.Data))
		}
	}
	p.close(s)
}

func (p *peer) sendMessage(typ string, data any) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		raw = b
	}
	select {
	case p.send <- message{Type: typ, Data: raw}:
	case <-p.done:
	default:
		slog.Warn("sfu signaling buffer full", "peer", p.id)
		p.ws.CloseNow()
	}
}

func (p *peer) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg := <-p.send:
			ctx, cancel := context.WithTimeout(p.ctx, writeWait)
			err := wsjson.Write(ctx, p.ws, msg)
			cancel()
			if err != nil {
				p.ws.CloseNow()
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(p.ctx, pongWait-pingPeriod)
			err := p.ws.Ping(ctx)
			cancel()
			if err != nil {
				p.ws.CloseNow()
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *peer) acceptOffer(offer webrtc.SessionDescription) error {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return err
	}
	p.room.mu.RLock()
	publications := make([]*publication, 0, len(p.room.publications))
	for _, pub := range p.room.publications {
		if pub.owner != p {
			publications = append(publications, pub)
		}
	}
	p.room.mu.RUnlock()
	for _, pub := range publications {
		if err := p.addSubscriptionLocked(pub); err != nil {
			return err
		}
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return err
	}
	p.ready = true
	p.dirty = false
	p.requestKeyframes()
	return nil
}

func (p *peer) announce() {
	r := p.room
	r.mu.Lock()
	peers := make([]*peer, 0, len(r.peers))
	ids := make([]string, 0, len(r.peers))
	profiles := make(map[string]peerProfile)
	for _, other := range r.peers {
		if other != p && other.announced {
			peers = append(peers, other)
			ids = append(ids, other.id)
			profiles[other.id] = other.profile
		}
	}
	p.announced = true
	p.sendMessage("joined", map[string]any{"id": p.id, "answer": p.pc.LocalDescription(), "peers": ids, "profiles": profiles})
	r.mu.Unlock()
	for _, other := range peers {
		other.sendMessage("peer_joined", map[string]any{"id": p.id, "profile": p.profile})
	}
}

func (p *peer) updateProfile(profile peerProfile) {
	r := p.room
	r.mu.Lock()
	p.profile = profile
	peers := make([]*peer, 0, len(r.peers))
	if p.announced {
		for _, other := range r.peers {
			if other != p && other.announced {
				peers = append(peers, other)
			}
		}
	}
	r.mu.Unlock()
	for _, other := range peers {
		other.sendMessage("state", map[string]any{"id": p.id, "profile": profile})
	}
}

func (p *peer) acceptAnswer(answer webrtc.SessionDescription) error {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return err
	}
	p.requestKeyframes()
	return p.sendOfferLocked()
}

func (p *peer) addSubscriptionLocked(pub *publication) error {
	if _, exists := p.subscriptions[pub.id]; exists {
		return nil
	}
	sender, err := p.pc.AddTrack(pub.local)
	if err != nil {
		return err
	}
	p.subscriptions[pub.id] = sender
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	return nil
}

func (p *peer) addSubscription(pub *publication) {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if err := p.addSubscriptionLocked(pub); err != nil {
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

func (p *peer) requestKeyframes() {
	p.room.mu.RLock()
	publications := make([]*publication, 0, len(p.subscriptions))
	for id := range p.subscriptions {
		if pub := p.room.publications[id]; pub != nil && pub.remote.Kind() == webrtc.RTPCodecTypeVideo {
			publications = append(publications, pub)
		}
	}
	p.room.mu.RUnlock()
	for _, pub := range publications {
		_ = pub.owner.pc.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(pub.remote.SSRC())},
		})
	}
}

func (p *peer) publish(track *webrtc.TrackRemote) {
	local, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, track.ID(), p.id)
	if err != nil {
		slog.Warn("sfu publish failed", "peer", p.id, "error", err)
		return
	}
	pub := &publication{id: p.id + ":" + track.ID(), owner: p, remote: track, local: local}
	r := p.room
	r.mu.Lock()
	r.publications[pub.id] = pub
	peers := make([]*peer, 0, len(r.peers))
	for _, other := range r.peers {
		if other != p {
			peers = append(peers, other)
		}
	}
	r.mu.Unlock()
	for _, other := range peers {
		other.addSubscription(pub)
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

func (p *peer) close(s *Server) {
	p.once.Do(func() {
		close(p.done)
		p.cancel()
		p.ws.CloseNow()
		p.pc.Close()
		r := p.room
		r.mu.Lock()
		delete(r.peers, p.id)
		peers := make([]*peer, 0, len(r.peers))
		if p.announced {
			for _, other := range r.peers {
				if other.announced {
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
		r.mu.Unlock()
		for _, other := range peers {
			other.sendMessage("peer_left", map[string]any{"id": p.id})
		}
		for _, pub := range publications {
			r.removePublication(pub)
		}
		s.removeEmptyRoom(r)
	})
}
