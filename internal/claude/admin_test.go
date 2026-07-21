package claude

import (
	"strings"
	"testing"
)

func TestAdminArgsRequestsSchema(t *testing.T) {
	a := Admin{Runtime: "docker", Image: "img", CredentialEnv: "CRED", Bare: true}
	args := a.args("create an agent named x", AdminContext{CurrentChannelID: "chan-1"})

	if !containsArg(args, "--json-schema") {
		t.Error("admin must request structured output")
	}
	if !containsArg(args, "--bare") {
		t.Error("Bare=true must pass --bare")
	}
	// The prompt must carry the request and the current channel for "here".
	last := args[len(args)-1]
	if !strings.Contains(last, "create an agent named x") || !strings.Contains(last, "chan-1") {
		t.Errorf("prompt missing request or channel context: %q", last)
	}
}

func TestAdminArgsNoBareForToken(t *testing.T) {
	a := Admin{Runtime: "docker", Image: "img", CredentialEnv: "CRED"} // Bare false
	if containsArg(a.args("x", AdminContext{}), "--bare") {
		t.Error("Bare=false must not pass --bare (a setup-token needs the full CLI)")
	}
}
