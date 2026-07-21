package turnserver

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/pion/turn/v5"

	"github.com/qrave1/sozvon/internal/config"
	"github.com/qrave1/sozvon/internal/logger"
)

func Start(cfg *config.Config) error {
	if cfg.TURN.RelayIP == "" {
		return fmt.Errorf("TURN_RELAY_IP is required")
	}

	ts, err := startServer(cfg.TURN.Port, cfg.TURN.RelayIP, cfg.TURN.Realm, cfg.TURN.Username, cfg.TURN.Password)
	if err != nil {
		return fmt.Errorf("failed to start TURN server: %w", err)
	}

	slog.Info("TURN server started", "port", cfg.TURN.Port, "relay", cfg.TURN.RelayIP, "user", cfg.TURN.Username)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down TURN server")
	return ts.Close()
}

func startServer(port, relayIP, realm, username, password string) (*turn.Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", port)
	if err != nil {
		return nil, err
	}

	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	tcpListener, err := net.Listen("tcp", port)
	if err != nil {
		return nil, err
	}

	server, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		LoggerFactory: logger.NewSlogLoggerFactory(),
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(relayIP),
					Address:      "0.0.0.0",
					MinPort:      50000,
					MaxPort:      51000,
				},
			},
		},
		ListenerConfigs: []turn.ListenerConfig{
			{
				Listener: tcpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: net.ParseIP(relayIP),
					Address:      "0.0.0.0",
					MinPort:      50000,
					MaxPort:      51000,
				},
			},
		},
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if ra.Username != username {
				return "", nil, false
			}
			return ra.Username, turn.GenerateAuthKey(ra.Username, realm, password), true
		},
	})
	if err != nil {
		return nil, err
	}

	return server, nil
}
