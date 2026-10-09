package sfu

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type message struct {
	Type    string          `json:"type"`
	Room    string          `json:"room,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Profile json.RawMessage `json:"profile,omitempty"`
}

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 20
)

type session struct {
	peerID string
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	send   chan message
	done   chan struct{}
	once   sync.Once
}

func newSession(w http.ResponseWriter, r *http.Request) (*session, error) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessageSize)
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		ws: ws, ctx: ctx, cancel: cancel,
		send: make(chan message, 256), done: make(chan struct{}),
	}, nil
}

func (s *session) read(ctx context.Context, msg *message) error {
	return wsjson.Read(ctx, s.ws, msg)
}

func (s *session) sendMessage(typ string, data any) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		raw = b
	}
	select {
	case s.send <- message{Type: typ, Data: raw}:
	case <-s.done:
	default:
		slog.Warn("sfu signaling buffer full", "peer", s.peerID)
		s.ws.CloseNow()
	}
}

func (s *session) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg := <-s.send:
			ctx, cancel := context.WithTimeout(s.ctx, writeWait)
			err := wsjson.Write(ctx, s.ws, msg)
			cancel()
			if err != nil {
				s.ws.CloseNow()
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, pongWait-pingPeriod)
			err := s.ws.Ping(ctx)
			cancel()
			if err != nil {
				s.ws.CloseNow()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		s.cancel()
		s.ws.CloseNow()
	})
}
