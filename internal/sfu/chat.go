package sfu

import (
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	chatMessageTTL    = 24 * time.Hour
	chatHistoryLimit  = 100
	chatRoomLimit     = 1000
	chatTextRuneLimit = 2000
)

type chatMessage struct {
	ID        string      `json:"id"`
	SenderID  string      `json:"senderId"`
	Name      string      `json:"name"`
	Color     string      `json:"color"`
	Text      string      `json:"text"`
	SentAt    time.Time   `json:"sentAt"`
	ExpiresAt time.Time   `json:"-"`
}

type chatStore struct {
	mu    sync.Mutex
	rooms map[string][]chatMessage
}

func newChatStore() *chatStore {
	return &chatStore{rooms: make(map[string][]chatMessage)}
}

func (s *chatStore) add(roomID string, profile peerProfile, senderID, text string, now time.Time) chatMessage {
	msg := chatMessage{
		ID: uuid.NewString(), SenderID: senderID, Name: profile.Name, Color: profile.Color,
		Text: text, SentAt: now.UTC(), ExpiresAt: now.Add(chatMessageTTL),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rooms[roomID]; !ok && len(s.rooms) >= chatRoomLimit {
		s.prune(now)
	}
	s.pruneRoom(roomID, now)
	history := append(s.rooms[roomID], msg)
	if len(history) > chatHistoryLimit {
		history = append([]chatMessage(nil), history[len(history)-chatHistoryLimit:]...)
	}
	s.rooms[roomID] = history
	if len(s.rooms) > chatRoomLimit {
		s.evictOldestRoom(roomID)
	}
	return msg
}

func (s *chatStore) history(roomID string, now time.Time) []chatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneRoom(roomID, now)
	return append([]chatMessage(nil), s.rooms[roomID]...)
}

func (s *chatStore) prune(now time.Time) {
	for roomID := range s.rooms {
		s.pruneRoom(roomID, now)
	}
}

func (s *chatStore) pruneRoom(roomID string, now time.Time) {
	history := s.rooms[roomID]
	firstValid := 0
	for firstValid < len(history) && !history[firstValid].ExpiresAt.After(now) {
		firstValid++
	}
	if firstValid == len(history) {
		delete(s.rooms, roomID)
	} else if firstValid > 0 {
		s.rooms[roomID] = append([]chatMessage(nil), history[firstValid:]...)
	}
}

func (s *chatStore) evictOldestRoom(except string) {
	oldestRoom := ""
	var oldest time.Time
	for roomID, history := range s.rooms {
		if roomID == except || len(history) == 0 {
			continue
		}
		if oldestRoom == "" || history[len(history)-1].SentAt.Before(oldest) {
			oldestRoom = roomID
			oldest = history[len(history)-1].SentAt
		}
	}
	if oldestRoom != "" {
		delete(s.rooms, oldestRoom)
	}
}

func parseChatText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > chatTextRuneLimit {
		return "", false
	}
	return text, true
}
