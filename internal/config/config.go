package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	HTTP  http
	Media media
	TURN  turn
}

type http struct {
	Port string `env:"PORT" envDefault:":8000"`
}

type media struct {
	UDPPort  uint16 `env:"MEDIA_UDP_PORT" envDefault:"40000"`
	PublicIP string `env:"MEDIA_PUBLIC_IP"`
}

type turn struct {
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
	return &c, nil
}
