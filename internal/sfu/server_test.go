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
	pc       *webrtc.PeerConnection
	ws       *websocket.Conn
	packets  chan *rtp.Packet
	events   chan message
	answer   webrtc.SessionDescription
	peers    []string
	readDone chan struct{}
}

func connectTestPeer(t *testing.T, wsURL, roomID string, kind webrtc.RTPCodecType, publish bool) (*testPeer, *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	p := &testPeer{pc: pc, packets: make(chan *rtp.Packet, 1), events: make(chan message, 8), readDone: make(chan struct{})}
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
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherComplete
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.ws = ws
	t.Cleanup(func() { ws.Close() })
	joinData, _ := json.Marshal(pc.LocalDescription())
	if err := ws.WriteJSON(message{Type: "join", Room: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var joined message
	if err := ws.ReadJSON(&joined); err != nil {
		t.Fatal(err)
	}
	if joined.Type != "joined" {
		t.Fatalf("expected joined, got %q", joined.Type)
	}
	var payload struct {
		Answer webrtc.SessionDescription `json:"answer"`
		Peers  []string                  `json:"peers"`
	}
	if err := json.Unmarshal(joined.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(payload.Answer); err != nil {
		t.Fatal(err)
	}
	p.answer = payload.Answer
	p.peers = payload.Peers
	ws.SetReadDeadline(time.Time{})
	go func() {
		defer close(p.readDone)
		for {
			var msg message
			if err := ws.ReadJSON(&msg); err != nil {
				return
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
			gather := webrtc.GatheringCompletePromise(pc)
			if pc.SetLocalDescription(answer) != nil {
				return
			}
			<-gather
			data, _ := json.Marshal(pc.LocalDescription())
			if ws.WriteJSON(message{Type: "answer", Data: data}) != nil {
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
	for _, line := range strings.Split(peer.answer.SDP, "\n") {
		if strings.Contains(line, "203.0.113.5 "+strconv.Itoa(int(port))) && strings.Contains(line, "typ host") {
			publicCandidate = true
		}
		if strings.HasPrefix(line, "a=candidate:") && strings.Contains(line, "typ host") && !strings.Contains(line, "203.0.113.5 ") {
			privateCandidate = true
		}
	}
	if !publicCandidate || !privateCandidate {
		t.Fatalf("answer must advertise public and local host candidates: %s", peer.answer.SDP)
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
