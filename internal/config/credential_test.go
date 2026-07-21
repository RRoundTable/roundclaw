package config

import "testing"

// The router decides whether it can run --bare from the credential type: --bare
// reads only an API key, so a setup-token must fall back to the full CLI.
func TestIsAPIKeyDistinguishesCredentialType(t *testing.T) {
	c := &ContainerConfig{OAuthTokenEnv: "OAUTH_TOK", APIKeyEnv: "API_KEY"}

	if !c.IsAPIKey(Credential{EnvName: "API_KEY", Value: "sk-api"}) {
		t.Error("an API-key credential must be reported as one, so --bare is used")
	}
	if c.IsAPIKey(Credential{EnvName: "OAUTH_TOK", Value: "sk-oauth"}) {
		t.Error("an OAuth setup-token must not be reported as an API key; --bare would fail with it")
	}
}
