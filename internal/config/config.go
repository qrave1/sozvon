package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	HTTP            http
	Media           media
	SFUExperimental bool   `env:"SFU_EXPERIMENTAL" envDefault:"false"`
	SFUUDPPort      uint16 `env:"SFU_UDP_PORT" envDefault:"40000"`
	SFUPublicIP     string `env:"SFU_PUBLIC_IP"`
	TURN            turn
}

type http struct {
	Port string `env:"PORT" envDefault:":8000"`
}

type media struct {
	Mode string `env:"MEDIA_MODE" envDefault:"mesh"`
}

type turn struct {
	Enabled  bool   `env:"TURN_ENABLED" envDefault:"true"`
	Port     string `env:"TURN_PORT" envDefault:":3478"`
	Realm    string `env:"TURN_REALM" envDefault:"sozvon"`
	Username string `env:"TURN_USERNAME" envDefault:"sozvon"`
	Password string `env:"TURN_PASSWORD" envDefault:"password"`
	RelayIP  string `env:"TURN_RELAY_IP"`
}

func New() (*Config, error) {
	c, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Media.Mode != "mesh" {
		return nil, fmt.Errorf("unsupported MEDIA_MODE %q (only mesh is available)", c.Media.Mode)
	}

	return &c, nil
}
