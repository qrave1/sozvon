package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/logging"
	"github.com/pion/turn/v3"
)

type Message struct {
	Type string          `json:"type"`
	Room string          `json:"room,omitempty"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Client struct {
	ID   string
	Conn *websocket.Conn
	Room *Room

	Name  string
	CamOn bool
	MicOn bool

	Send chan Message

	mu     sync.Mutex
	closed bool
}

func (c *Client) send(msg Message) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return false
	}

	select {
	case c.Send <- msg:
		return true
	default:
		slog.Warn("client send buffer full", "client", c.ID)
		return false
	}
}

func (c *Client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}
	c.closed = true
	close(c.Send)
	c.Conn.Close()
}

type Room struct {
	ID string

	Clients map[string]*Client

	mu sync.RWMutex
}

func (r *Room) AddClient(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.Clients[c.ID] = c
	c.Room = r
}

func (r *Room) RemoveClient(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.Clients, id)
}

func (r *Room) Broadcast(msg Message, except string) {
	r.mu.RLock()
	clients := make([]*Client, 0, len(r.Clients))
	for id, client := range r.Clients {
		if id == except {
			continue
		}
		clients = append(clients, client)
	}
	r.mu.RUnlock()

	for _, client := range clients {
		client.send(msg)
	}
}

func (r *Room) GetClient(id string) (*Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, ok := r.Clients[id]
	return client, ok
}

func (r *Room) ListClients(except string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.Clients))
	for id := range r.Clients {
		if id == except {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (r *Room) ListClientsInfo(except string) []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]PeerInfo, 0, len(r.Clients))
	for id, client := range r.Clients {
		if id == except {
			continue
		}
		infos = append(infos, client.peerInfo())
	}
	return infos
}

func (r *Room) IsEmpty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.Clients) == 0
}

type Server struct {
	Rooms map[string]*Room

	mu sync.RWMutex
}

func NewServer() *Server {
	return &Server{
		Rooms: make(map[string]*Room),
	}
}

func (s *Server) GetRoom(id string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.Rooms[id]
	if ok {
		return room
	}

	room = &Room{
		ID:      id,
		Clients: make(map[string]*Client),
	}

	s.Rooms[id] = room

	return room
}

func (s *Server) RemoveRoom(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.Rooms, id)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 20
)

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}

	conn.SetReadLimit(maxMessageSize)

	client := &Client{
		ID:   uuid.NewString(),
		Conn: conn,
		Send: make(chan Message, 256),
	}

	slog.Info("client connected", "client", client.ID)

	go client.writeLoop()
	go s.readLoop(client)
}

func (c *Client) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteJSON(msg); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) readLoop(c *Client) {
	defer func() {
		s.disconnect(c)
	}()

	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		return c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		var msg Message

		if err := c.Conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("read error", "client", c.ID, "error", err)
			}
			return
		}

		switch msg.Type {
		case "join":
			s.handleJoin(c, msg)

		case "state":
			s.handleState(c, msg)

		case "offer":
			s.forward(c, msg)

		case "answer":
			s.forward(c, msg)

		case "candidate":
			s.forward(c, msg)

		default:
			slog.Warn("unknown message type", "type", msg.Type)
		}
	}
}

type PeerInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	CamOn bool   `json:"camOn"`
	MicOn bool   `json:"micOn"`
	Info  string `json:"info,omitempty"`
}

func (c *Client) peerInfo() PeerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PeerInfo{ID: c.ID, Name: c.Name, CamOn: c.CamOn, MicOn: c.MicOn}
}

func (s *Server) handleJoin(c *Client, msg Message) {
	if msg.Room == "" {
		return
	}
	if c.Room != nil {
		return
	}

	var info struct {
		Name  string `json:"name"`
		CamOn bool   `json:"camOn"`
		MicOn bool   `json:"micOn"`
	}
	if len(msg.Data) > 0 {
		_ = json.Unmarshal(msg.Data, &info)
	}

	c.mu.Lock()
	c.Name = info.Name
	c.CamOn = info.CamOn
	c.MicOn = info.MicOn
	c.mu.Unlock()

	room := s.GetRoom(msg.Room)
	room.AddClient(c)

	slog.Info("client joined room", "client", c.ID, "room", room.ID)

	existing := room.ListClientsInfo(c.ID)

	c.send(Message{
		Type: "joined",
		From: "server",
		Data: mustMarshal(map[string]any{
			"id":    c.ID,
			"peers": existing,
		}),
	})

	if len(existing) > 0 {
		room.Broadcast(
			Message{
				Type: "user_joined",
				From: c.ID,
				Data: mustMarshal(c.peerInfo()),
			},
			c.ID,
		)
	}
}

func (s *Server) handleState(c *Client, msg Message) {
	var info struct {
		Name  string `json:"name"`
		CamOn bool   `json:"camOn"`
		MicOn bool   `json:"micOn"`
	}
	if len(msg.Data) > 0 {
		_ = json.Unmarshal(msg.Data, &info)
	}

	c.mu.Lock()
	c.Name = info.Name
	c.CamOn = info.CamOn
	c.MicOn = info.MicOn
	c.mu.Unlock()

	if c.Room == nil {
		return
	}

	c.Room.Broadcast(
		Message{
			Type: "state",
			From: c.ID,
			Data: mustMarshal(c.peerInfo()),
		},
		c.ID,
	)
}

func (s *Server) forward(c *Client, msg Message) {
	if c.Room == nil || msg.To == "" {
		return
	}

	target, ok := c.Room.GetClient(msg.To)
	if !ok {
		return
	}

	msg.From = c.ID

	target.send(msg)
}

func (s *Server) disconnect(c *Client) {
	if c.Room != nil {
		room := c.Room

		room.RemoveClient(c.ID)

		room.Broadcast(
			Message{
				Type: "user_left",
				From: c.ID,
			},
			c.ID,
		)

		if room.IsEmpty() {
			s.RemoveRoom(room.ID)
			slog.Info("room removed", "room", room.ID)
		}
	}

	c.close()

	slog.Info("client disconnected", "client", c.ID)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
}

// --- slog-адаптер для pion/logging ---

type slogLogger struct {
	l *slog.Logger
}

func (s slogLogger) Trace(msg string) { s.l.Debug(msg) }
func (s slogLogger) Tracef(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
func (s slogLogger) Debug(msg string) { s.l.Debug(msg) }
func (s slogLogger) Debugf(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
func (s slogLogger) Info(msg string)                          { s.l.Info(msg) }
func (s slogLogger) Infof(format string, args ...interface{}) { s.l.Info(fmt.Sprintf(format, args...)) }
func (s slogLogger) Warn(msg string)                          { s.l.Warn(msg) }
func (s slogLogger) Warnf(format string, args ...interface{}) { s.l.Warn(fmt.Sprintf(format, args...)) }
func (s slogLogger) Error(msg string)                         { s.l.Error(msg) }
func (s slogLogger) Errorf(format string, args ...interface{}) {
	s.l.Error(fmt.Sprintf(format, args...))
}

type slogLoggerFactory struct {
	l *slog.Logger
}

func (f slogLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return slogLogger{l: f.l.With("component", scope)}
}

func startTURNServer(listen, relayIP, realm, username, password string) (*turn.Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}

	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	server, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		LoggerFactory: slogLoggerFactory{l: slog.Default()},
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: net.ParseIP(relayIP),
					Address:      "0.0.0.0",
				},
			},
		},
		AuthHandler: func(u, r string, src net.Addr) ([]byte, bool) {
			if u != username {
				return nil, false
			}
			return turn.GenerateAuthKey(username, realm, password), true
		},
	})
	if err != nil {
		return nil, err
	}

	return server, nil
}

type Config struct {
	HTTPPort string `env:"HTTP_PORT" envDefault:":8000"`

	TURNEnabled  bool   `env:"TURN_ENABLED" envDefault:"true"`
	TURNPort     string `env:"TURN_PORT" envDefault:":3478"`
	TURNRealm    string `env:"TURN_REALM" envDefault:"sozvon"`
	TURNUsername string `env:"TURN_USERNAME" envDefault:"sozvon"`
	TURNPassword string `env:"TURN_PASSWORD" envDefault:"sozvon123"`
	TURNRelayIP  string `env:"TURN_RELAY_IP"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		slog.Error("failed to parse config", "error", err)
		os.Exit(1)
	}

	var turnServer *turn.Server
	if cfg.TURNEnabled {
		if cfg.TURNRelayIP == "" {
			slog.Error("TURN relay IP is empty")
			os.Exit(-1)
		}

		ts, err := startTURNServer(cfg.TURNPort, cfg.TURNRelayIP, cfg.TURNRealm, cfg.TURNUsername, cfg.TURNPassword)
		if err != nil {
			slog.Error("failed to start TURN server", "error", err)
			os.Exit(1)
		}
		turnServer = ts
		defer turnServer.Close()
		slog.Info("TURN server started", "port", cfg.TURNPort, "relay", cfg.TURNRelayIP, "user", cfg.TURNUsername)
	}

	server := NewServer()

	http.Handle("/", http.FileServer(http.Dir("./web")))
	http.HandleFunc("/ws", server.handleWS)
	http.HandleFunc("/turn-config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		json.NewEncoder(w).Encode(map[string]any{
			"urls":       []string{"turn:" + host + cfg.TURNPort},
			"username":   cfg.TURNUsername,
			"credential": cfg.TURNPassword,
		})
	})

	slog.Info("server started", "port", cfg.HTTPPort)

	if err := http.ListenAndServe(cfg.HTTPPort, nil); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
