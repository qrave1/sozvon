package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
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
		log.Printf("client %s send buffer full", c.ID)
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
		log.Println(err)
		return
	}

	conn.SetReadLimit(maxMessageSize)

	client := &Client{
		ID:   uuid.NewString(),
		Conn: conn,
		Send: make(chan Message, 256),
	}

	log.Printf("client connected: %s", client.ID)

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
				log.Printf("read error (%s): %v", c.ID, err)
			}
			return
		}

		switch msg.Type {
		case "join":
			s.handleJoin(c, msg)

		case "offer":
			s.forward(c, msg)

		case "answer":
			s.forward(c, msg)

		case "candidate":
			s.forward(c, msg)

		default:
			log.Printf("unknown message type: %s", msg.Type)
		}
	}
}

func (s *Server) handleJoin(c *Client, msg Message) {
	if msg.Room == "" {
		return
	}
	if c.Room != nil {
		return
	}

	room := s.GetRoom(msg.Room)
	room.AddClient(c)

	log.Printf("%s joined room %s", c.ID, room.ID)

	existing := room.ListClients(c.ID)

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
			},
			c.ID,
		)
	}
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
			log.Printf("room %s removed", room.ID)
		}
	}

	c.close()

	log.Printf("client disconnected: %s", c.ID)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
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
		Realm: realm,
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

func detectRelayIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return "127.0.0.1"
}

func main() {
	httpAddr := flag.String("http", ":8080", "адрес HTTP/сигналинг сервера")
	turnAddr := flag.String("turn", ":3478", "адрес TURN сервера (udp)")
	turnRealm := flag.String("realm", "sozvon", "TURN realm")
	turnUser := flag.String("turn-user", "sozvon", "TURN username")
	turnPass := flag.String("turn-pass", "sozvon123", "TURN password")
	relayIP := flag.String("relay-ip", "", "внешний IP для TURN relay (по умолчанию: авто)")
	enableTURN := flag.Bool("enable-turn", true, "запустить встроенный TURN сервер")
	flag.Parse()

	var turnServer *turn.Server
	if *enableTURN {
		rip := *relayIP
		if rip == "" {
			rip = detectRelayIP()
		}
		ts, err := startTURNServer(*turnAddr, rip, *turnRealm, *turnUser, *turnPass)
		if err != nil {
			log.Fatalf("failed to start TURN server: %v", err)
		}
		turnServer = ts
		defer turnServer.Close()
		log.Printf("TURN server started on %s (relay %s, user=%s)", *turnAddr, rip, *turnUser)
	}
	_ = turnServer

	server := NewServer()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/ws", server.handleWS)
	http.HandleFunc("/turn-config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		json.NewEncoder(w).Encode(map[string]any{
			"urls":       []string{"turn:" + host + *turnAddr},
			"username":   *turnUser,
			"credential": *turnPass,
		})
	})

	log.Printf("server started on %s", *httpAddr)

	if err := http.ListenAndServe(*httpAddr, nil); err != nil {
		log.Fatal(err)
	}
}
