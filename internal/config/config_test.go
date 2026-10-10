package config

import (
	"os"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PORT", "MEDIA_UDP_PORT", "MEDIA_PUBLIC_IP", "TURN_PORT", "TURN_REALM",
		"TURN_USERNAME", "TURN_PASSWORD", "TURN_RELAY_IP",
	} {
		value, exists := os.LookupEnv(key)
		t.Cleanup(func() {
			if exists {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		})
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNew(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		clearConfigEnv(t)
		cfg, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HTTP.Port != ":8000" || cfg.Media.UDPPort != 40000 || cfg.Media.PublicIP != "" {
			t.Fatal("unexpected HTTP or media defaults")
		}
		if cfg.TURN.Port != ":3478" || cfg.TURN.Realm != "sozvon" || cfg.TURN.Username != "sozvon" || cfg.TURN.Password != "password" || cfg.TURN.RelayIP != "" {
			t.Fatal("unexpected TURN defaults")
		}
	})
	t.Run("environment overrides", func(t *testing.T) {
		clearConfigEnv(t)
		for key, value := range map[string]string{
			"PORT": ":18003", "MEDIA_UDP_PORT": "40003", "MEDIA_PUBLIC_IP": "203.0.113.10",
			"TURN_PORT": ":13478", "TURN_REALM": "test", "TURN_USERNAME": "user",
			"TURN_PASSWORD": "test-password", "TURN_RELAY_IP": "203.0.113.20",
		} {
			t.Setenv(key, value)
		}
		cfg, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HTTP.Port != ":18003" || cfg.Media.UDPPort != 40003 || cfg.Media.PublicIP != "203.0.113.10" {
			t.Fatal("HTTP or media environment override was not applied")
		}
		if cfg.TURN.Port != ":13478" || cfg.TURN.Realm != "test" || cfg.TURN.Username != "user" || cfg.TURN.Password != "test-password" || cfg.TURN.RelayIP != "203.0.113.20" {
			t.Fatal("TURN environment override was not applied")
		}
	})
	for _, port := range []string{"invalid", "65536", "-1"} {
		t.Run("invalid media port "+port, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("MEDIA_UDP_PORT", port)
			if _, err := New(); err == nil {
				t.Fatal("expected invalid media port to be rejected")
			}
		})
	}
}
