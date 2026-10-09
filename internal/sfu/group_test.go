package sfu

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func pumpTrack(t *testing.T, track *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint16
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sequence++
				_ = track.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960},
					Payload: []byte{0xf8, 0xff, 0xfe},
				})
			}
		}
	}()
}

func waitPublications(t *testing.T, server *Server, roomID string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := server.getRoomForTest(roomID); r != nil {
			r.mu.RLock()
			got := len(r.publications)
			r.mu.RUnlock()
			if got == count {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("room %q did not reach %d publications", roomID, count)
}

func TestGroupEveryParticipantReceivesExistingAudioVideo(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	var peers []*testPeer
	for i := range 4 {
		p, _ := connectTestPeerWithKinds(t, "ws"+strings.TrimPrefix(httpServer.URL, "http"), "group", webrtc.RTPCodecTypeAudio, true, []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo})
		peers = append(peers, p)
		for _, sender := range p.pc.GetSenders() {
			pumpTrack(t, sender.Track().(*webrtc.TrackLocalStaticRTP))
		}
		waitPublications(t, server, "group", (i+1)*2)
	}
	for _, p := range peers {
		expectPublisherTracks(t, p, peers)
	}
	peers[1].ws.CloseNow()
	remaining := []*testPeer{peers[0], peers[2], peers[3]}
	waitPublications(t, server, "group", 6)
	newcomer, _ := connectTestPeerWithKinds(t, "ws"+strings.TrimPrefix(httpServer.URL, "http"), "group", webrtc.RTPCodecTypeAudio, true, []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo})
	for _, sender := range newcomer.pc.GetSenders() {
		pumpTrack(t, sender.Track().(*webrtc.TrackLocalStaticRTP))
	}
	waitPublications(t, server, "group", 8)
	expectPublisherTracks(t, newcomer, append(remaining, newcomer))
	for _, p := range remaining {
		expectPublisherTracks(t, p, []*testPeer{p, newcomer})
	}
}

func expectPublisherTracks(t *testing.T, subscriber *testPeer, publishers []*testPeer) {
	t.Helper()
	wanted := make(map[string]bool)
	for _, publisher := range publishers {
		if publisher != subscriber {
			for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
				wanted[publisher.id+":"+kind.String()] = true
			}
		}
	}
	timeout := time.After(5 * time.Second)
	for len(wanted) != 0 {
		select {
		case track := <-subscriber.tracks:
			key := track.StreamID() + ":" + track.Kind().String()
			if !wanted[key] {
				t.Fatalf("unexpected track %s for %s", key, subscriber.id)
			}
			delete(wanted, key)
		case <-timeout:
			t.Fatalf("participant %s is missing tracks: %v", subscriber.id, wanted)
		}
	}
}

func TestSubscriberKeyframeRequestReachesPublisher(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	publisher, outgoing := connectTestPeer(t, wsURL, "feedback", webrtc.RTPCodecTypeVideo, true)
	pumpTrack(t, outgoing)
	waitPublications(t, server, "feedback", 1)
	subscriber, _ := connectTestPeer(t, wsURL, "feedback", webrtc.RTPCodecTypeVideo, false)
	var incoming *webrtc.TrackRemote
	select {
	case incoming = <-subscriber.tracks:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber did not receive video")
	}
	feedback := make(chan *rtcp.PictureLossIndication, 16)
	go func() {
		for {
			packets, _, err := publisher.pc.GetSenders()[0].ReadRTCP()
			if err != nil {
				return
			}
			for _, packet := range packets {
				if pli, ok := packet.(*rtcp.PictureLossIndication); ok {
					select {
					case feedback <- pli:
					case <-publisher.done:
						return
					}
				}
			}
		}
	}()
	if err := subscriber.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{
		SenderSSRC: 123456, MediaSSRC: uint32(incoming.SSRC()),
	}}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(3 * time.Second)
	for {
		select {
		case pli := <-feedback:
			if pli.SenderSSRC == 123456 {
				want := publisher.pc.GetSenders()[0].GetParameters().Encodings[0].SSRC
				if pli.MediaSSRC != uint32(want) {
					t.Fatalf("forwarded media SSRC = %d, want %d", pli.MediaSSRC, want)
				}
				return
			}
		case <-timeout:
			t.Fatal("subscriber PLI did not reach publisher")
		}
	}
}

func TestInboundTransportExtensionsAreNotForwarded(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	_, outgoing := connectTestPeer(t, wsURL, "extensions", webrtc.RTPCodecTypeAudio, true)
	subscriber, _ := connectTestPeer(t, wsURL, "extensions", webrtc.RTPCodecTypeAudio, false)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(5 * time.Second)
	var sequence uint16
	for {
		select {
		case packet := <-subscriber.packets:
			if packet.GetExtension(14) != nil {
				t.Fatal("inbound transport-scoped extension leaked to a different connection")
			}
			return
		case <-ticker.C:
			sequence++
			packet := &rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960},
				Payload: []byte{0xf8, 0xff, 0xfe},
			}
			if err := packet.SetExtension(14, []byte{1, 2}); err != nil {
				t.Fatal(err)
			}
			_ = outgoing.WriteRTP(packet)
		case <-timeout:
			t.Fatal("subscriber did not receive RTP")
		}
	}
}
