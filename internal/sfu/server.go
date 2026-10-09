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
	"sync"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

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
	type peerRef struct {
		room *room
		peer *peer
	}
	peers := make([]peerRef, 0)
	for _, r := range s.rooms {
		r.mu.RLock()
		for _, p := range r.peers {
			peers = append(peers, peerRef{room: r, peer: p})
		}
		r.mu.RUnlock()
	}
	s.mu.Unlock()
	for _, ref := range peers {
		s.removePeer(ref.room, ref.peer)
	}
	if s.udpConn != nil {
		return s.udpConn.Close()
	}
	return nil
}

func (s *Server) addPeer(id string, p *peer, profile peerProfile) *room {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[id]
	if r == nil {
		r = &room{
			id: id, peers: make(map[string]*peer), profiles: make(map[string]peerProfile),
			announced: make(map[string]bool), publications: make(map[string]*publication),
		}
		s.rooms[id] = r
	}
	r.mu.Lock()
	r.peers[p.id] = p
	r.profiles[p.id] = profile
	r.mu.Unlock()
	return r
}

func (s *Server) removePeer(r *room, p *peer) {
	if !p.close() {
		return
	}
	p.session.close()
	peers, publications, announced := r.removePeer(p)
	if announced {
		for _, other := range peers {
			other.sendMessage("peer_left", map[string]any{"id": p.id})
		}
	}
	for _, pub := range publications {
		r.removePublication(pub)
	}
	s.removeEmptyRoom(r)
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

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	session, err := newSession(w, r)
	if err != nil {
		return
	}
	defer session.close()

	var first message
	joinCtx, joinCancel := context.WithTimeout(session.ctx, pongWait)
	err = session.read(joinCtx, &first)
	joinCancel()
	if err != nil || first.Type != "join" || first.Room == "" || len(first.Room) > 128 {
		return
	}

	var offer webrtc.SessionDescription
	if err := json.Unmarshal(first.Data, &offer); err != nil || offer.Type != webrtc.SDPTypeOffer {
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		slog.Error("sfu peer creation failed", "error", err)
		return
	}

	peerID := uuid.NewString()
	session.peerID = peerID
	p := &peer{
		id: peerID, pc: pc, session: session, done: make(chan struct{}),
		subscriptions: make(map[string]*webrtc.RTPSender),
	}
	room := s.addPeer(first.Room, p, parseProfile(first.Profile))

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		room.publish(p, track)
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			p.sendMessage("candidate", candidate.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed {
			slog.Warn("sfu peer connection failed", "peer", p.id, "room", room.id)
			go s.removePeer(room, p)
		} else if state == webrtc.PeerConnectionStateClosed {
			go s.removePeer(room, p)
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Info("sfu ICE state changed", "peer", p.id, "room", room.id, "state", state.String())
	})

	go session.writeLoop()
	if err := p.acceptOffer(offer); err != nil {
		slog.Warn("sfu offer failed", "peer", p.id, "error", err)
		s.removePeer(room, p)
		return
	}
	if !room.announce(p) {
		return
	}
	if err := p.activate(room); err != nil {
		slog.Warn("sfu initial subscriptions failed", "peer", p.id, "error", err)
		s.removePeer(room, p)
		return
	}

	for {
		var msg message
		if err := session.read(session.ctx, &msg); err != nil {
			if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway && !errors.Is(err, context.Canceled) {
				slog.Info("sfu websocket read ended", "peer", p.id, "room", room.id, "error", err)
			}
			break
		}
		switch msg.Type {
		case "answer":
			var answer webrtc.SessionDescription
			if json.Unmarshal(msg.Data, &answer) == nil && answer.Type == webrtc.SDPTypeAnswer {
				if err := p.acceptAnswer(answer); err != nil {
					slog.Warn("sfu answer failed", "peer", p.id, "error", err)
					s.removePeer(room, p)
					return
				}
				room.requestSubscribedKeyframes(p)
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
			room.updateProfile(p, parseProfile(msg.Data))
		case "refresh_video":
			room.requestPublishedKeyframes(p)
		}
	}
	s.removePeer(room, p)
}
