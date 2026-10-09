package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/qrave1/sozvon/internal/config"
	"github.com/qrave1/sozvon/internal/sfu"
	"github.com/qrave1/sozvon/internal/signaling"
	"github.com/qrave1/sozvon/internal/turnserver"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.New()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	cmd := &cli.Command{
		Name:  "sozvon",
		Usage: "WebRTC signaling and TURN server",
		Action: func(ctx context.Context, c *cli.Command) error {
			return runServer(cfg)
		},
		Commands: []*cli.Command{
			{
				Name:  "turn",
				Usage: "Start TURN server",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return turnserver.Start(cfg)
				},
			},
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("app failed", "error", err)
		os.Exit(1)
	}
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func runServer(cfg *config.Config) error {
	publicIP := cfg.SFUPublicIP
	if publicIP == "" {
		publicIP = cfg.TURN.RelayIP
	}
	sfuServer, err := sfu.NewServerWithUDP(cfg.SFUUDPPort, publicIP)
	if err != nil {
		return err
	}
	defer sfuServer.Close()
	slog.Info("SFU UDP listener started", "port", cfg.SFUUDPPort, "public_ip", publicIP)
	slog.Info("server started", "port", cfg.HTTP.Port)
	return http.ListenAndServe(cfg.HTTP.Port, newHandler(cfg, signaling.NewServer(), sfuServer))
}

func newHandler(cfg *config.Config, meshServer *signaling.Server, sfuServer *sfu.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", noCache(http.FileServer(http.Dir("./web"))))
	mux.HandleFunc("/ws", meshServer.HandleWS)
	mux.HandleFunc("/sfu.html", http.NotFound)
	mux.Handle("/sfu", noCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./web/sfu.html")
	})))
	mux.HandleFunc("/sfu/ws", sfuServer.HandleWS)

	if cfg.TURN.RelayIP != "" {
		mux.HandleFunc("/turn-config", func(w http.ResponseWriter, r *http.Request) {
			addr := net.JoinHostPort(cfg.TURN.RelayIP, strings.TrimPrefix(cfg.TURN.Port, ":"))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"urls":       []string{"turn:" + addr, "turn:" + addr + "?transport=tcp"},
				"username":   cfg.TURN.Username,
				"credential": cfg.TURN.Password,
			})
		})
	}

	return mux
}
