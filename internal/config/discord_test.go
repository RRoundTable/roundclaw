package config

import "testing"

// The permission setting must fail closed. An unrecognised or empty value
// opening the commands to everyone would silently hand destructive, billable
// commands to every member of a server.
func TestCommandPermissionFailsClosed(t *testing.T) {
	for _, value := range []string{"", "manage_guild", "nonsense", "MANAGE_GUILD"} {
		bits := DiscordConfig{CommandPermission: value}.CommandPermissionBits()
		if bits == nil {
			t.Errorf("%q produced no restriction; it must fall back to manage_guild", value)
			continue
		}
		if *bits != permManageGuild {
			t.Errorf("%q produced bits %d, want manage_guild (%d)", value, *bits, permManageGuild)
		}
	}
}

func TestCommandPermissionExplicitValues(t *testing.T) {
	if bits := (DiscordConfig{CommandPermission: "everyone"}).CommandPermissionBits(); bits != nil {
		t.Errorf("everyone produced bits %d, want no restriction", *bits)
	}
	bits := DiscordConfig{CommandPermission: "administrator"}.CommandPermissionBits()
	if bits == nil || *bits != permAdministrator {
		t.Errorf("administrator produced %v, want %d", bits, permAdministrator)
	}
}
