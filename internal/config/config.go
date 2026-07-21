package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	HTTP http
	TURN turn
}

type http struct {
	Port string `env:"PORT" envDefault:":8000"`
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

	return &c, nil
}
