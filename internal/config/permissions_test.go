package config

import "testing"

func withAllowList(roles, users []string) DiscordConfig {
	c := &Config{Discord: DiscordConfig{AllowedRoles: roles, AllowedUsers: users}}
	c.applyDefaults()
	return c.Discord
}

// With no allow-list the check must not restrict anything, or configuring
// nothing would lock everyone out.
func TestNoAllowListPermitsEverything(t *testing.T) {
	d := withAllowList(nil, nil)
	if d.CommandsRestricted() {
		t.Error("an empty allow-list reported as restricted")
	}
	for _, cmd := range []string{"ask", "stop", "agent", "schedule"} {
		if !d.PermitsCommand(cmd, "nobody", nil) {
			t.Errorf("%s was refused with no allow-list configured", cmd)
		}
	}
}

func TestAllowedRoleAndUserPass(t *testing.T) {
	d := withAllowList([]string{"role-ops"}, []string{"user-alice"})

	if !d.PermitsCommand("ask", "user-bob", []string{"role-ops"}) {
		t.Error("a member with an allowed role was refused")
	}
	if !d.PermitsCommand("ask", "user-alice", nil) {
		t.Error("an explicitly allowed user was refused")
	}
	if d.PermitsCommand("ask", "user-bob", []string{"role-everyone"}) {
		t.Error("a member with no allowed role was permitted")
	}
	if d.PermitsCommand("ask", "user-bob", nil) {
		t.Error("a caller with no roles at all was permitted")
	}
}

// Read-only commands stay open. Refusing to say what an agent is doing makes
// the bot look broken to whoever is best placed to notice a problem, and it
// costs nothing to answer.
func TestReadOnlyCommandsBypassTheAllowList(t *testing.T) {
	d := withAllowList([]string{"role-ops"}, nil)

	for _, cmd := range []string{"status", "workflow", "agents"} {
		if !d.PermitsCommand(cmd, "stranger", nil) {
			t.Errorf("read-only command %s was refused", cmd)
		}
	}
	// Everything else can spend money or destroy work in progress.
	for _, cmd := range []string{"ask", "stop", "steer", "agent", "schedule"} {
		if d.PermitsCommand(cmd, "stranger", nil) {
			t.Errorf("%s was permitted to an unlisted caller", cmd)
		}
	}
}
