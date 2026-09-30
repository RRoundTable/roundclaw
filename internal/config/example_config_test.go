package config

import "testing"

func TestExampleLoadsLifecycleRules(t *testing.T) {
	cfg, err := Load("../../roundclaw.example.yaml")
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if cfg.Routines.Lifecycle.RecurrenceWindow.Hours() != 336 || cfg.Routines.Lifecycle.SweepEvery.Minutes() != 15 {
		t.Fatalf("durations not read: %+v", cfg.Routines.Lifecycle)
	}
}
