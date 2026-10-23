// Package config reads the server configuration from the environment, with
// an optional .env file for local development (CASE_SPEC: the Anthropic key
// comes from ANTHROPIC_API_KEY in .env).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the runtime configuration of cmd/server.
type Config struct {
	HTTPAddr     string // HTTP_ADDR, default ":8080"
	DBPath       string // DB_PATH, default "data/campaign.db"
	ProviderURL  string // PROVIDER_URL, default "http://localhost:9090"
	CORSOrigins  string // CORS_ALLOWED_ORIGINS, comma separated; empty = dev defaults
	AnthropicKey string // ANTHROPIC_API_KEY; empty = mock LLM mode
	// MockStreamDelay slows the mock composer's token stream so the typing
	// effect is visible in the UI (MOCK_STREAM_DELAY_MS, default 15).
	MockStreamDelay time.Duration
}

// LLMMode is "anthropic" when a key is configured, "mock" otherwise.
func (c Config) LLMMode() string {
	if c.AnthropicKey != "" {
		return "anthropic"
	}
	return "mock"
}

// FromEnv reads Config from the process environment.
func FromEnv() (Config, error) {
	c := Config{
		HTTPAddr:     envOr("HTTP_ADDR", ":8080"),
		DBPath:       envOr("DB_PATH", filepath.Join("data", "campaign.db")),
		ProviderURL:  envOr("PROVIDER_URL", "http://localhost:9090"),
		CORSOrigins:  os.Getenv("CORS_ALLOWED_ORIGINS"),
		AnthropicKey: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
	}
	ms, err := strconv.Atoi(envOr("MOCK_STREAM_DELAY_MS", "15"))
	if err != nil || ms < 0 {
		return Config{}, fmt.Errorf("MOCK_STREAM_DELAY_MS must be a non-negative integer, got %q", os.Getenv("MOCK_STREAM_DELAY_MS"))
	}
	c.MockStreamDelay = time.Duration(ms) * time.Millisecond
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadDotEnv sets variables from the first existing file in paths, without
// overriding variables that are already set (the real environment, e.g.
// docker compose, always wins). It returns the file it loaded, or "" when
// none exists. Supported syntax: KEY=VALUE lines, blank lines, # comments,
// an optional "export " prefix and single or double quotes around values.
func LoadDotEnv(paths ...string) (string, error) {
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			key, val, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" || strings.ContainsAny(key, " \t") {
				return "", fmt.Errorf("%s:%d: expected KEY=VALUE", p, n)
			}
			val = unquote(strings.TrimSpace(val))
			if _, set := os.LookupEnv(key); set {
				continue
			}
			if err := os.Setenv(key, val); err != nil {
				return "", err
			}
		}
		if err := sc.Err(); err != nil {
			return "", err
		}
		return p, nil
	}
	return "", nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	// Strip an inline comment from unquoted values: KEY=value # note
	if i := strings.Index(v, " #"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return v
}
