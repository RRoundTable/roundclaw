package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnvFile reads a .env file next to the config file and exports any
// variable that is not already set.
//
// Precedence is deliberate: a variable already present in the real environment
// always wins. A file on disk must never be able to silently override what an
// operator, a systemd unit, or a container runtime injected — that is how a
// stale local .env ends up quietly pointing production at the wrong
// credentials.
//
// A missing file is not an error: .env is a local-development convenience, and
// deployments are expected to inject the environment directly.
func LoadEnvFile(configPath string) error {
	path := filepath.Join(filepath.Dir(configPath), ".env")

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")

		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value", path, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: empty key", path, line)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, unquote(strings.TrimSpace(value))); err != nil {
			return fmt.Errorf("%s:%d: set %s: %w", path, line, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// unquote strips one matching pair of surrounding quotes. Tokens are routinely
// pasted with quotes, and a literal quote character in a credential would
// otherwise fail authentication in a way that is tedious to diagnose.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
