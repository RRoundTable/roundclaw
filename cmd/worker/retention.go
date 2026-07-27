package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/workspace"
)

// runRetention sweeps old history until ctx is cancelled.
//
// It lives in the worker rather than the gateway because the worker is the
// process that can afford to be busy: the gateway answers /status, and a sweep
// holding SQLite's write lock is exactly what should not sit in front of that.
func runRetention(ctx context.Context, cfg *config.Config, reg *registry.Store,
	stores *store.Registry, log *slog.Logger) {
	if !cfg.Retention.Enabled() {
		log.Info("history retention is off; transcripts and turns are kept forever")
		return
	}
	log.Info("history retention enabled",
		"transcript_days", cfg.Retention.TranscriptDays,
		"turn_days", cfg.Retention.TurnDays,
		"upload_days", cfg.Retention.UploadDays,
		"interval", cfg.Retention.Interval)

	// Once at startup, so a long-running deployment does not wait a full
	// interval after a restart before reclaiming anything.
	pruneOnce(ctx, cfg, reg, stores, log)

	ticker := time.NewTicker(cfg.Retention.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneOnce(ctx, cfg, reg, stores, log)
		}
	}
}

func pruneOnce(ctx context.Context, cfg *config.Config, reg *registry.Store,
	stores *store.Registry, log *slog.Logger) {
	agents, err := reg.List(ctx)
	if err != nil {
		log.Warn("retention sweep could not list agents", "error", err)
		return
	}

	now := time.Now()
	logsBefore := now.AddDate(0, 0, -cfg.Retention.TranscriptDays)
	turnsBefore := now.AddDate(0, 0, -cfg.Retention.TurnDays)
	// A zero setting means "keep forever" for that dimension, so push its
	// cutoff far enough back that nothing matches.
	if cfg.Retention.TranscriptDays == 0 {
		logsBefore = time.Unix(0, 0)
	}
	if cfg.Retention.TurnDays == 0 {
		turnsBefore = time.Unix(0, 0)
	}
	uploadsBefore := now.AddDate(0, 0, -cfg.Retention.UploadDays)

	var total store.Pruned
	var uploads int64
	for _, a := range agents {
		// Staged uploads are files, not rows, and an agent that has never run
		// still has a staging directory if someone sent it something — so this
		// runs whether or not the store opens.
		if cfg.Retention.UploadDays > 0 {
			n, err := workspace.PruneStaging(cfg, a.ID, uploadsBefore)
			if err != nil {
				log.Warn("could not sweep staged uploads", "agent", a.ID, "error", err)
			}
			uploads += int64(n)
		}

		st, err := stores.Get(a.ID)
		if err != nil {
			continue // never run, nothing to prune
		}
		p, err := st.Prune(ctx, logsBefore, turnsBefore)
		if err != nil {
			log.Warn("retention sweep failed for an agent", "agent", a.ID, "error", err)
			continue
		}
		total.Logs += p.Logs
		total.Turns += p.Turns
		total.Keys += p.Keys
	}

	if total.Logs+total.Turns+total.Keys+uploads > 0 {
		log.Info("pruned history",
			"transcript_rows", total.Logs, "turns", total.Turns,
			"idempotency_keys", total.Keys, "staged_uploads", uploads)
	}
}
