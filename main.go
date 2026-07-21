package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/qrave1/sozvon/internal/config"
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

func runServer(cfg *config.Config) error {
	server := signaling.NewServer()

	http.Handle("/", http.FileServer(http.Dir("./web")))
	http.HandleFunc("/ws", server.HandleWS)

	if cfg.TURN.RelayIP != "" {
		http.HandleFunc("/turn-config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"urls":       []string{"turn:" + cfg.TURN.RelayIP, "turn:" + cfg.TURN.RelayIP + "?transport=tcp"},
				"username":   cfg.TURN.Username,
				"credential": cfg.TURN.Password,
			})
		})
	}

	slog.Info("server started", "port", cfg.HTTP.Port)
	return http.ListenAndServe(cfg.HTTP.Port, nil)
}
