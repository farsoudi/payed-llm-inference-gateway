package config

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr               string
	OllamaURL                string
	OllamaModel              string
	PostgresURL              string
	Network                  string
	PayTo                    string
	FacilitatorURL           string
	FacilitatorAuthorization string
	PricePerTokenMicro       int64
	MinTopupMicro            int64
	SafetyMaxTokens          int
	CheckpointTokens         int
	ReloadLead               time.Duration
	RequestTimeout           time.Duration
	MaxBodyBytes             int64
	DefaultRatePerMin        int
	DefaultConcurrency       int
}

func Defaults() Config {
	return Config{
		ListenAddr:         ":8080",
		OllamaURL:          "http://127.0.0.1:11434",
		OllamaModel:        "qwen2.5",
		Network:            "base-sepolia",
		FacilitatorURL:     "https://api.cdp.coinbase.com/platform/v2/x402",
		PricePerTokenMicro: 5,
		MinTopupMicro:      500_000,
		SafetyMaxTokens:    16_384,
		CheckpointTokens:   25,
		ReloadLead:         8 * time.Second,
		RequestTimeout:     10 * time.Minute,
		MaxBodyBytes:       1 << 20,
		DefaultRatePerMin:  60,
		DefaultConcurrency: 1,
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		if err := applyFile(&cfg, path); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.PostgresURL == "" {
		return errors.New("POSTGRES_URL is required")
	}
	if c.PayTo == "" {
		return errors.New("PAY_TO is required")
	}
	if !validEVMAddress(c.PayTo) {
		return errors.New("PAY_TO must be a non-zero 20-byte EVM address")
	}
	if c.OllamaURL == "" || c.OllamaModel == "" {
		return errors.New("OLLAMA_URL and OLLAMA_MODEL are required")
	}
	if c.PricePerTokenMicro <= 0 || c.MinTopupMicro <= 0 {
		return errors.New("price and minimum top-up must be positive")
	}
	if c.SafetyMaxTokens <= 0 || c.CheckpointTokens <= 0 {
		return errors.New("safety and checkpoint token limits must be positive")
	}
	if c.ReloadLead <= 0 || c.RequestTimeout <= 0 || c.MaxBodyBytes <= 0 {
		return errors.New("reload lead, request timeout, and max body size must be positive")
	}
	if c.DefaultRatePerMin <= 0 || c.DefaultConcurrency <= 0 {
		return errors.New("default limits must be positive")
	}
	if c.Network != "base-sepolia" && c.Network != "base" {
		return fmt.Errorf("unsupported network %q: use base-sepolia or base", c.Network)
	}
	facilitator, err := url.Parse(c.FacilitatorURL)
	if err != nil || facilitator.Host == "" || (facilitator.Scheme != "https" && facilitator.Scheme != "http") {
		return errors.New("FACILITATOR_URL must be an HTTP(S) URL")
	}
	if c.Network == "base" && facilitator.Scheme != "https" {
		return errors.New("FACILITATOR_URL must use HTTPS on Base mainnet")
	}
	return nil
}

func validEVMAddress(value string) bool {
	if len(value) != 42 || !strings.EqualFold(value[:2], "0x") {
		return false
	}
	b, err := hex.DecodeString(value[2:])
	if err != nil || len(b) != 20 {
		return false
	}
	for _, part := range b {
		if part != 0 {
			return true
		}
	}
	return false
}

func applyFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid config line %q", line)
		}
		if err := set(cfg, strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), "\"'")); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	return nil
}

func applyEnv(cfg *Config) error {
	for key := range envKeys {
		if value, ok := os.LookupEnv(key); ok {
			if err := set(cfg, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

var envKeys = map[string]struct{}{
	"LISTEN_ADDR": {}, "OLLAMA_URL": {}, "OLLAMA_MODEL": {}, "POSTGRES_URL": {},
	"NETWORK": {}, "PAY_TO": {}, "FACILITATOR_URL": {}, "FACILITATOR_AUTHORIZATION": {}, "PRICE_PER_TOKEN_MICRO": {},
	"MIN_TOPUP_MICRO": {}, "SAFETY_MAX_TOKENS": {}, "CHECKPOINT_TOKENS": {},
	"RELOAD_LEAD": {}, "REQUEST_TIMEOUT": {}, "MAX_BODY_BYTES": {},
	"DEFAULT_RATE_PER_MIN": {}, "DEFAULT_CONCURRENCY": {},
}

func set(c *Config, key, value string) error {
	int64Value := func() (int64, error) { return strconv.ParseInt(value, 10, 64) }
	intValue := func() (int, error) { n, err := strconv.Atoi(value); return n, err }
	durationValue := func() (time.Duration, error) { return time.ParseDuration(value) }
	var err error
	switch key {
	case "LISTEN_ADDR":
		c.ListenAddr = value
	case "OLLAMA_URL":
		c.OllamaURL = strings.TrimRight(value, "/")
	case "OLLAMA_MODEL":
		c.OllamaModel = value
	case "POSTGRES_URL":
		c.PostgresURL = value
	case "NETWORK":
		c.Network = strings.ToLower(value)
	case "PAY_TO":
		c.PayTo = value
	case "FACILITATOR_URL":
		c.FacilitatorURL = strings.TrimRight(value, "/")
	case "FACILITATOR_AUTHORIZATION":
		c.FacilitatorAuthorization = value
	case "PRICE_PER_TOKEN_MICRO":
		c.PricePerTokenMicro, err = int64Value()
	case "MIN_TOPUP_MICRO":
		c.MinTopupMicro, err = int64Value()
	case "SAFETY_MAX_TOKENS":
		c.SafetyMaxTokens, err = intValue()
	case "CHECKPOINT_TOKENS":
		c.CheckpointTokens, err = intValue()
	case "RELOAD_LEAD":
		c.ReloadLead, err = durationValue()
	case "REQUEST_TIMEOUT":
		c.RequestTimeout, err = durationValue()
	case "MAX_BODY_BYTES":
		c.MaxBodyBytes, err = int64Value()
	case "DEFAULT_RATE_PER_MIN":
		c.DefaultRatePerMin, err = intValue()
	case "DEFAULT_CONCURRENCY":
		c.DefaultConcurrency, err = intValue()
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	if err != nil {
		return fmt.Errorf("invalid %s: %w", key, err)
	}
	return nil
}
