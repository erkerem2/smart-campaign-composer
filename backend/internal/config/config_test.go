package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# comment
ANTHROPIC_API_KEY=sk-test-123
export PROVIDER_URL="http://provider:9090"
CORS_ALLOWED_ORIGINS='http://a.test, http://b.test'
HTTP_ADDR=:9999 # inline comment
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "PROVIDER_URL", "CORS_ALLOWED_ORIGINS", "EMPTY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HTTP_ADDR", ":7000") // already set: the real environment wins

	got, err := LoadDotEnv(filepath.Join(dir, "missing.env"), path)
	if err != nil || got != path {
		t.Fatalf("LoadDotEnv = %q, %v", got, err)
	}
	want := map[string]string{
		"ANTHROPIC_API_KEY":    "sk-test-123",
		"PROVIDER_URL":         "http://provider:9090",
		"CORS_ALLOWED_ORIGINS": "http://a.test, http://b.test",
		"HTTP_ADDR":            ":7000",
		"EMPTY":                "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMMode() != "anthropic" || c.HTTPAddr != ":7000" || c.MockStreamDelay != 15*time.Millisecond {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadDotEnvMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadDotEnv(filepath.Join(dir, "nope")); err != nil || got != "" {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	bad := filepath.Join(dir, "bad.env")
	_ = os.WriteFile(bad, []byte("NOT A PAIR\n"), 0o600)
	if _, err := LoadDotEnv(bad); err == nil {
		t.Fatal("malformed line accepted")
	}
}

func TestFromEnvDefaultsAndValidation(t *testing.T) {
	for _, k := range []string{"HTTP_ADDR", "DB_PATH", "PROVIDER_URL", "ANTHROPIC_API_KEY", "MOCK_STREAM_DELAY_MS"} {
		t.Setenv(k, "")
	}
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.ProviderURL != "http://localhost:9090" || c.LLMMode() != "mock" {
		t.Fatalf("defaults = %+v", c)
	}
	t.Setenv("MOCK_STREAM_DELAY_MS", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("negative delay accepted")
	}
}
