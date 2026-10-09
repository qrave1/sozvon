package sfu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"
)

func TestChatTTLAndHistoryLimit(t *testing.T) {
	store := newChatStore()
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < chatHistoryLimit+1; i++ {
		store.add("room", peerProfile{Name: "Имя"}, "peer", "сообщение", now.Add(time.Duration(i)*time.Second))
	}
	history := store.history("room", now.Add(time.Duration(chatHistoryLimit)*time.Second))
	if len(history) != chatHistoryLimit {
		t.Fatalf("history length = %d, want %d", len(history), chatHistoryLimit)
	}
	if history[0].SentAt != now.Add(time.Second) {
		t.Fatalf("oldest retained message time = %s, want %s", history[0].SentAt, now.Add(time.Second))
	}
	if got := store.history("room", now.Add(time.Duration(chatHistoryLimit)*time.Second+chatMessageTTL)); len(got) != 0 {
		t.Fatalf("expired history length = %d, want 0", len(got))
	}
}

func TestChatBroadcastHistoryAndRoomIsolation(t *testing.T) {
	server := NewServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWS))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	profile := peerProfile{Name: "Отправитель", Color: "#3498db"}
	first, _ := connectTestPeer(t, wsURL, "chat-a", webrtc.RTPCodecTypeAudio, false, profile)
	second, _ := connectTestPeer(t, wsURL, "chat-a", webrtc.RTPCodecTypeAudio, false)
	outsider, _ := connectTestPeer(t, wsURL, "chat-b", webrtc.RTPCodecTypeAudio, false)

	data, _ := json.Marshal(map[string]string{"text": "  привет  "})
	if err := wsjson.Write(context.Background(), first.ws, message{Type: "chat_send", Data: data}); err != nil {
		t.Fatal(err)
	}
	firstMessage := nextChatMessage(t, first)
	secondMessage := nextChatMessage(t, second)
	if firstMessage != secondMessage {
		t.Fatalf("sender and recipient got different messages: %+v / %+v", firstMessage, secondMessage)
	}
	if firstMessage.Text != "привет" || firstMessage.SenderID != first.id || firstMessage.Name != profile.Name || firstMessage.Color != profile.Color {
		t.Fatalf("chat message = %+v", firstMessage)
	}
	select {
	case msg := <-outsider.events:
		if msg.Type == "chat_message" {
			t.Fatal("chat message crossed room boundary")
		}
	default:
	}

	late, _ := connectTestPeer(t, wsURL, "chat-a", webrtc.RTPCodecTypeAudio, false)
	if len(late.chatHistory) != 1 || late.chatHistory[0] != firstMessage {
		t.Fatalf("late join history = %+v, want [%+v]", late.chatHistory, firstMessage)
	}
}

func nextChatMessage(t *testing.T, p *testPeer) chatMessage {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-p.events:
			if msg.Type != "chat_message" {
				continue
			}
			var result chatMessage
			if err := json.Unmarshal(msg.Data, &result); err != nil {
				t.Fatal(err)
			}
			return result
		case <-deadline:
			t.Fatal("timed out waiting for chat message")
			return chatMessage{}
		}
	}
}

func TestParseChatText(t *testing.T) {
	if _, ok := parseChatText("  \n "); ok {
		t.Fatal("whitespace-only message was accepted")
	}
	if _, ok := parseChatText(strings.Repeat("я", chatTextRuneLimit+1)); ok {
		t.Fatal("oversized message was accepted")
	}
	if got, ok := parseChatText("  сообщение  "); !ok || got != "сообщение" {
		t.Fatalf("parseChatText() = %q, %v", got, ok)
	}
}
