package sfu

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type testPeer struct {
	id         string
	pc         *webrtc.PeerConnection
	ws         *websocket.Conn
	packets    chan *rtp.Packet
	events     chan message
	candidates chan webrtc.ICECandidateInit
	send       chan message
	done       chan struct{}
	answer     webrtc.SessionDescription
	peers      []string
	profiles   map[string]peerProfile
	readDone   chan struct{}
}

func connectTestPeer(t *testing.T, wsURL, roomID string, kind webrtc.RTPCodecType, publish bool, profile ...peerProfile) (*testPeer, *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	p := &testPeer{
		pc: pc, packets: make(chan *rtp.Packet, 1), events: make(chan message, 8),
		candidates: make(chan webrtc.ICECandidateInit, 32), send: make(chan message, 64),
		done: make(chan struct{}), readDone: make(chan struct{}),
	}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		data, _ := json.Marshal(candidate.ToJSON())
		select {
		case p.send <- message{Type: "candidate", Data: data}:
		case <-p.done:
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		packet, _, err := track.ReadRTP()
		if err == nil {
			p.packets <- packet
		}
	})
	var outgoing *webrtc.TrackLocalStaticRTP
	if publish {
		codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		trackID := "audio"
		if kind == webrtc.RTPCodecTypeVideo {
			codec = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
			trackID = "video"
		}
		outgoing, err = webrtc.NewTrackLocalStaticRTP(
			codec, trackID, "local",
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTrack(outgoing); err != nil {
			t.Fatal(err)
		}
	} else if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.ws = ws
	t.Cleanup(func() { ws.Close() })
	t.Cleanup(func() { close(p.done) })
	joinData, _ := json.Marshal(pc.LocalDescription())
	join := message{Type: "join", Room: roomID, Data: joinData}
	if len(profile) > 0 {
		join.Profile, _ = json.Marshal(profile[0])
	}
	if err := ws.WriteJSON(join); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			select {
			case msg := <-p.send:
				if ws.WriteJSON(msg) != nil {
					return
				}
			case <-p.done:
				return
			}
		}
	}()
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var joined message
	var pending []webrtc.ICECandidateInit
	for joined.Type != "joined" {
		var msg message
		if err := ws.ReadJSON(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "candidate" {
			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal(msg.Data, &candidate); err != nil {
				t.Fatal(err)
			}
			pending = append(pending, candidate)
			p.candidates <- candidate
		} else if msg.Type == "joined" {
			joined = msg
		} else {
			t.Fatalf("expected joined or candidate, got %q", msg.Type)
		}
	}
	var payload struct {
		ID       string                    `json:"id"`
		Answer   webrtc.SessionDescription `json:"answer"`
		Peers    []string                  `json:"peers"`
		Profiles map[string]peerProfile    `json:"profiles"`
	}
	if err := json.Unmarshal(joined.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(payload.Answer); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range pending {
		if err := pc.AddICECandidate(candidate); err != nil {
			t.Fatal(err)
		}
	}
	p.answer = payload.Answer
	p.id = payload.ID
	p.peers = payload.Peers
	p.profiles = payload.Profiles
	ws.SetReadDeadline(time.Time{})
	go func() {
		defer close(p.readDone)
		for {
			var msg message
			if err := ws.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == "candidate" {
				var candidate webrtc.ICECandidateInit
				if json.Unmarshal(msg.Data, &candidate) != nil || pc.AddICECandidate(candidate) != nil {
					return
				}
				select {
				case p.candidates <- candidate:
				default:
				}
				continue
			}
			if msg.Type != "offer" {
				select {
				case p.events <- msg:
				default:
				}
				continue
			}
			var remoteOffer webrtc.SessionDescription
			if json.Unmarshal(msg.Data, &remoteOffer) != nil || pc.SetRemoteDescription(remoteOffer) != nil {
				return
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return
			}
			if pc.SetLocalDescription(answer) != nil {
				return
			}
			data, _ := json.Marshal(pc.LocalDescription())
			select {
			case p.send <- message{Type: "answer", Data: data}:
			case <-p.done:
				return
			}
		}
	}()
	return p, outgoing
}

func TestRoomRoster(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	first, _ := connectTestPeer(t, wsURL, "roster", webrtc.RTPCodecTypeAudio, false)
	second, _ := connectTestPeer(t, wsURL, "roster", webrtc.RTPCodecTypeAudio, false)
	if len(second.peers) != 1 {
		t.Fatalf("joined roster has %d peers, want 1", len(second.peers))
	}
	select {
	case msg := <-first.events:
		if msg.Type != "peer_joined" {
			t.Fatalf("got %q, want peer_joined", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first peer did not receive peer_joined")
	}
	second.ws.Close()
	select {
	case msg := <-first.events:
		if msg.Type != "peer_left" {
			t.Fatalf("got %q, want peer_left", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first peer did not receive peer_left")
	}
}

func TestPeerProfiles(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	firstProfile := peerProfile{Name: "Аня", MicOn: true, CamOn: false, Color: "#3498db"}
	first, _ := connectTestPeer(t, wsURL, "profiles", webrtc.RTPCodecTypeAudio, false, firstProfile)
	secondProfile := peerProfile{Name: "Борис", MicOn: false, CamOn: true, Color: "#e74c3c"}
	second, _ := connectTestPeer(t, wsURL, "profiles", webrtc.RTPCodecTypeAudio, false, secondProfile)
	if got := second.profiles[first.id]; got != firstProfile {
		t.Fatalf("joined profile = %+v, want %+v", got, firstProfile)
	}
	select {
	case msg := <-first.events:
		var data struct {
			ID      string      `json:"id"`
			Profile peerProfile `json:"profile"`
		}
		if msg.Type != "peer_joined" || json.Unmarshal(msg.Data, &data) != nil || data.ID != second.id || data.Profile != secondProfile {
			t.Fatalf("peer_joined = %+v, data = %+v", msg, data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first peer did not receive profile")
	}
	updated := peerProfile{Name: "Борис", MicOn: true, CamOn: false, ScreenShare: true, Color: "#e74c3c"}
	data, _ := json.Marshal(updated)
	if err := second.ws.WriteJSON(message{Type: "state", Data: data}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-first.events:
		var state struct {
			ID      string      `json:"id"`
			Profile peerProfile `json:"profile"`
		}
		if msg.Type != "state" || json.Unmarshal(msg.Data, &state) != nil || state.ID != second.id || state.Profile != updated {
			t.Fatalf("state = %+v, data = %+v", msg, state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first peer did not receive state update")
	}
}

func TestTrickleICEConnection(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	localCandidates := make(chan webrtc.ICECandidateInit, 32)
	connected := make(chan struct{}, 1)
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			localCandidates <- candidate.ToJSON()
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	data, _ := json.Marshal(pc.LocalDescription())
	if err := ws.WriteJSON(message{Type: "join", Room: "trickle", Data: data}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case candidate := <-localCandidates:
				data, _ := json.Marshal(candidate)
				if ws.WriteJSON(message{Type: "candidate", Data: data}) != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	events := make(chan message, 32)
	go func() {
		for {
			var msg message
			if ws.ReadJSON(&msg) != nil {
				return
			}
			events <- msg
		}
	}()
	var pending []webrtc.ICECandidateInit
	answered := false
	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-connected:
			if !answered {
				t.Fatal("ICE connected before answer")
			}
			return
		case msg := <-events:
			switch msg.Type {
			case "joined":
				var payload struct {
					Answer webrtc.SessionDescription `json:"answer"`
				}
				if err := json.Unmarshal(msg.Data, &payload); err != nil {
					t.Fatal(err)
				}
				if err := pc.SetRemoteDescription(payload.Answer); err != nil {
					t.Fatal(err)
				}
				answered = true
				for _, candidate := range pending {
					if err := pc.AddICECandidate(candidate); err != nil {
						t.Fatal(err)
					}
				}
			case "candidate":
				var candidate webrtc.ICECandidateInit
				if err := json.Unmarshal(msg.Data, &candidate); err != nil {
					t.Fatal(err)
				}
				if answered {
					if err := pc.AddICECandidate(candidate); err != nil {
						t.Fatal(err)
					}
				} else {
					pending = append(pending, candidate)
				}
			}
		case <-timeout:
			t.Fatalf("trickle ICE did not connect, state = %s", pc.ConnectionState())
		}
	}
}

func TestPublicICECandidate(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.LocalAddr().(*net.UDPAddr).Port)
	listener.Close()
	server, err := NewServerWithUDP(port, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	peer, _ := connectTestPeer(t, wsURL, "ice", webrtc.RTPCodecTypeAudio, false)
	publicCandidate := false
	privateCandidate := false
	checkCandidate := func(line string) {
		if strings.Contains(line, "203.0.113.5 "+strconv.Itoa(int(port))) && strings.Contains(line, "typ host") {
			publicCandidate = true
		}
		if strings.Contains(line, "candidate:") && strings.Contains(line, "typ host") && !strings.Contains(line, "203.0.113.5 ") {
			privateCandidate = true
		}
	}
	for _, line := range strings.Split(peer.answer.SDP, "\n") {
		checkCandidate(line)
	}
	timeout := time.After(5 * time.Second)
	for !publicCandidate || !privateCandidate {
		select {
		case candidate := <-peer.candidates:
			checkCandidate(candidate.Candidate)
		case <-timeout:
			t.Fatalf("answer must advertise public and local host candidates: %s", peer.answer.SDP)
		}
	}
}

func TestForwardsRTPWithinRoom(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	publisher, outgoing := connectTestPeer(t, wsURL, "room-a", webrtc.RTPCodecTypeAudio, true)
	otherRoom, _ := connectTestPeer(t, wsURL, "room-b", webrtc.RTPCodecTypeAudio, false)
	subscriber, _ := connectTestPeer(t, wsURL, "room-a", webrtc.RTPCodecTypeAudio, false)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(8 * time.Second)
	var sequence uint16
	for {
		select {
		case packet := <-subscriber.packets:
			if len(packet.Payload) == 0 {
				t.Fatal("received empty RTP packet")
			}
			select {
			case <-otherRoom.packets:
				t.Fatal("RTP crossed room boundary")
			default:
			}
			publisher.ws.Close()
			deadline := time.Now().Add(2 * time.Second)
			for {
				r := server.getRoomForTest("room-a")
				r.mu.RLock()
				count := len(r.publications)
				r.mu.RUnlock()
				if count == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("publication remained after publisher disconnected")
				}
				time.Sleep(10 * time.Millisecond)
			}
			return
		case <-ticker.C:
			sequence++
			_ = outgoing.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960},
				Payload: []byte{0xf8, 0xff, 0xfe},
			})
		case <-timeout:
			t.Fatal("subscriber did not receive RTP")
		}
	}
}

func TestLateSubscriber(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	_, outgoing := connectTestPeer(t, wsURL, "late", webrtc.RTPCodecTypeAudio, true)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint16
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sequence++
				_ = outgoing.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960},
					Payload: []byte{0xf8, 0xff, 0xfe},
				})
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		r := server.getRoomForTest("late")
		if r != nil {
			r.mu.RLock()
			count := len(r.publications)
			r.mu.RUnlock()
			if count > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("publisher did not register its track")
		}
		time.Sleep(20 * time.Millisecond)
	}

	subscriber, _ := connectTestPeer(t, wsURL, "late", webrtc.RTPCodecTypeAudio, false)
	select {
	case <-subscriber.packets:
	case <-time.After(8 * time.Second):
		t.Fatal("late subscriber did not receive RTP")
	}
}

func TestForwardsVideoRTP(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	_, outgoing := connectTestPeer(t, wsURL, "video", webrtc.RTPCodecTypeVideo, true)
	subscriber, _ := connectTestPeer(t, wsURL, "video", webrtc.RTPCodecTypeVideo, false)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(8 * time.Second)
	var sequence uint16
	for {
		select {
		case packet := <-subscriber.packets:
			if len(packet.Payload) == 0 {
				t.Fatal("received empty video RTP packet")
			}
			return
		case <-ticker.C:
			sequence++
			_ = outgoing.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 3000},
				Payload: []byte{0x10, 0x00, 0x00},
			})
		case <-timeout:
			t.Fatal("subscriber did not receive video RTP")
		}
	}
}

func (s *Server) getRoomForTest(id string) *room {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[id]
}
