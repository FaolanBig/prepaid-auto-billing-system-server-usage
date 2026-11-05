package config

import (
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddress   string
	DatabaseHost  string
	DatabasePort  string
	DatabaseName  string
	DatabaseUser  string
	DatabasePass  string
	InfluxURL     string
	InfluxOrg     string
	InfluxBucket  string
	InfluxToken   string
	ShellyURL     string
	ShellyUser    string
	ShellyPass    string
	PollInterval  time.Duration
	AdminUsername string
	AdminPassword string
	CookieSecret  string
	CookieSecure  bool
	AdminEmail    string
	SMTPHost      string
	SMTPPort      string
	SMTPUsername  string
	SMTPPassword  string
	SMTPFrom      string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddress:   env("HTTP_ADDRESS", ":8080"),
		DatabaseHost:  env("POSTGRES_HOST", "postgres"),
		DatabasePort:  env("POSTGRES_PORT", "5432"),
		DatabaseName:  env("POSTGRES_DB", "powerbilling"),
		DatabaseUser:  env("POSTGRES_USER", "powerbilling"),
		DatabasePass:  os.Getenv("POSTGRES_PASSWORD"),
		InfluxURL:     env("INFLUX_URL", "http://influxdb:8086"),
		InfluxOrg:     env("INFLUX_ORG", "powerbilling"),
		InfluxBucket:  env("INFLUX_BUCKET", "power"),
		InfluxToken:   os.Getenv("INFLUX_TOKEN"),
		ShellyURL:     os.Getenv("SHELLY_URL"),
		ShellyUser:    os.Getenv("SHELLY_USERNAME"),
		ShellyPass:    os.Getenv("SHELLY_PASSWORD"),
		AdminUsername: env("ADMIN_USERNAME", "admin"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		CookieSecret:  os.Getenv("COOKIE_SECRET"),
		AdminEmail:    os.Getenv("ADMIN_EMAIL"),
		SMTPHost:      os.Getenv("SMTP_HOST"),
		SMTPPort:      env("SMTP_PORT", "587"),
		SMTPUsername:  os.Getenv("SMTP_USERNAME"),
		SMTPPassword:  os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:      os.Getenv("SMTP_FROM"),
	}

	cookieSecure, err := strconv.ParseBool(env("COOKIE_SECURE", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("COOKIE_SECURE must be true or false")
	}
	cfg.CookieSecure = cookieSecure
	pollSeconds, err := strconv.Atoi(env("SHELLY_POLL_SECONDS", "60"))
	if err != nil || pollSeconds < 5 || pollSeconds > 3600 {
		return Config{}, fmt.Errorf("SHELLY_POLL_SECONDS must be an integer from 5 through 3600")
	}
	cfg.PollInterval = time.Duration(pollSeconds) * time.Second

	if cfg.DatabasePass == "" || strings.Contains(strings.ToLower(cfg.DatabasePass), "replace-me") {
		return Config{}, fmt.Errorf("POSTGRES_PASSWORD must be set to a unique value")
	}
	if cfg.InfluxToken == "" || strings.Contains(strings.ToLower(cfg.InfluxToken), "replace-me") {
		return Config{}, fmt.Errorf("INFLUX_TOKEN must be set to a unique value")
	}
	if len(cfg.AdminPassword) < 16 || strings.Contains(strings.ToLower(cfg.AdminPassword), "replace-me") {
		return Config{}, fmt.Errorf("ADMIN_PASSWORD must be a unique value of at least 16 characters")
	}
	if cfg.AdminUsername == "" || strings.Contains(cfg.AdminUsername, "|") {
		return Config{}, fmt.Errorf("ADMIN_USERNAME must be non-empty and cannot contain a vertical bar")
	}
	if len(cfg.CookieSecret) < 32 || strings.Contains(strings.ToLower(cfg.CookieSecret), "replace-me") {
		return Config{}, fmt.Errorf("COOKIE_SECRET must be a unique value of at least 32 characters")
	}
	if cfg.SMTPHost == "" || cfg.SMTPFrom == "" || cfg.AdminEmail == "" {
		return Config{}, fmt.Errorf("SMTP_HOST, SMTP_FROM, and ADMIN_EMAIL are required for billing notifications")
	}
	if (cfg.SMTPUsername == "") != (cfg.SMTPPassword == "") {
		return Config{}, fmt.Errorf("SMTP_USERNAME and SMTP_PASSWORD must either both be set or both be empty")
	}
	smtpPort, err := strconv.Atoi(cfg.SMTPPort)
	if err != nil || smtpPort < 1 || smtpPort > 65535 {
		return Config{}, fmt.Errorf("SMTP_PORT must be a valid TCP port")
	}
	for name, address := range map[string]string{
		"ADMIN_EMAIL": cfg.AdminEmail,
		"SMTP_FROM":   cfg.SMTPFrom,
	} {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Address != address {
			return Config{}, fmt.Errorf("%s must be a valid email address", name)
		}
	}
	if cfg.ShellyURL != "" {
		parsed, err := url.ParseRequestURI(cfg.ShellyURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" || parsed.User != nil {
			return Config{}, fmt.Errorf("SHELLY_URL must be an HTTP or HTTPS RPC URL")
		}
	}
	if (cfg.ShellyUser == "") != (cfg.ShellyPass == "") {
		return Config{}, fmt.Errorf("SHELLY_USERNAME and SHELLY_PASSWORD must either both be set or both be empty")
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
