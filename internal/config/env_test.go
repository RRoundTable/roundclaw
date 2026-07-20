package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return filepath.Join(dir, "roundclaw.yaml")
}

// The real environment must win. A stale local .env silently overriding an
// injected credential is how a deployment ends up pointing somewhere wrong.
func TestLoadEnvFileDoesNotOverrideRealEnvironment(t *testing.T) {
	t.Setenv("ROUNDCLAW_TEST_EXISTING", "from-environment")
	configPath := writeEnv(t, "ROUNDCLAW_TEST_EXISTING=from-file\n")

	if err := LoadEnvFile(configPath); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("ROUNDCLAW_TEST_EXISTING"); got != "from-environment" {
		t.Errorf("value = %q, want the environment's value", got)
	}
}

func TestLoadEnvFileSetsUnsetVariables(t *testing.T) {
	configPath := writeEnv(t, `
# a comment
ROUNDCLAW_TEST_PLAIN=plain-value
export ROUNDCLAW_TEST_EXPORTED=exported-value
ROUNDCLAW_TEST_DQUOTED="double quoted"
ROUNDCLAW_TEST_SQUOTED='single quoted'
ROUNDCLAW_TEST_EQUALS=key=with=equals

ROUNDCLAW_TEST_SPACED  =  spaced
`)
	for _, k := range []string{
		"ROUNDCLAW_TEST_PLAIN", "ROUNDCLAW_TEST_EXPORTED", "ROUNDCLAW_TEST_DQUOTED",
		"ROUNDCLAW_TEST_SQUOTED", "ROUNDCLAW_TEST_EQUALS", "ROUNDCLAW_TEST_SPACED",
	} {
		t.Setenv(k, "") // registers cleanup
		os.Unsetenv(k)
	}

	if err := LoadEnvFile(configPath); err != nil {
		t.Fatalf("load: %v", err)
	}

	want := map[string]string{
		"ROUNDCLAW_TEST_PLAIN":    "plain-value",
		"ROUNDCLAW_TEST_EXPORTED": "exported-value",
		"ROUNDCLAW_TEST_DQUOTED":  "double quoted",
		"ROUNDCLAW_TEST_SQUOTED":  "single quoted",
		// A token containing '=' must survive: Cut splits on the first '=' only.
		"ROUNDCLAW_TEST_EQUALS": "key=with=equals",
		"ROUNDCLAW_TEST_SPACED": "spaced",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// .env is a local convenience; deployments inject the environment directly, so
// its absence must not be an error.
func TestLoadEnvFileIgnoresMissingFile(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "roundclaw.yaml")); err != nil {
		t.Errorf("missing .env returned %v, want nil", err)
	}
}

func TestLoadEnvFileRejectsMalformedLine(t *testing.T) {
	configPath := writeEnv(t, "THIS_LINE_HAS_NO_EQUALS\n")
	if err := LoadEnvFile(configPath); err == nil {
		t.Error("malformed line was accepted")
	}
}
