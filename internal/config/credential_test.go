package config

import (
	"strings"
	"testing"
)

// The router runs `claude --bare`, which reads only an API key, so its
// credential resolution must not hand back an OAuth setup-token even when one is
// set — doing so is what made every routing call fail with "Not logged in".
func TestResolveRouterCredentialRequiresAPIKey(t *testing.T) {
	c := &ContainerConfig{OAuthTokenEnv: "OAUTH_TOK", APIKeyEnv: "API_KEY"}

	// Only an OAuth token is set: agents would accept it, the router must not.
	only := map[string]string{"OAUTH_TOK": "sk-oauth"}
	lookup := func(k string) (string, bool) { v, ok := only[k]; return v, ok }

	if _, err := c.ResolveRouterCredential(lookup); err == nil {
		t.Error("router accepted an OAuth token, but --bare cannot use one")
	}
	// And the error must point at the fix.
	_, err := c.ResolveRouterCredential(lookup)
	if err == nil || !strings.Contains(err.Error(), "API_KEY") {
		t.Errorf("error = %v, want it to name the API key env var", err)
	}

	// With the API key set, it resolves to that — never the OAuth token.
	both := map[string]string{"OAUTH_TOK": "sk-oauth", "API_KEY": "sk-api"}
	cred, err := c.ResolveRouterCredential(func(k string) (string, bool) { v, ok := both[k]; return v, ok })
	if err != nil {
		t.Fatalf("resolve with API key set: %v", err)
	}
	if cred.EnvName != "API_KEY" || cred.Value != "sk-api" {
		t.Errorf("resolved %+v, want the API key even though an OAuth token is also set", cred)
	}
}
