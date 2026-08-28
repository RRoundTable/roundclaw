// Command gateway runs roundclaw's inbound edges: the Discord listener and the
// HTTP API. It owns no agent execution — it admits requests, signals Temporal,
// and answers status questions straight from SQLite.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/adapter"
	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

func main() {
	configPath := flag.String("config", "roundclaw.yaml", "path to the roundclaw config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, log); err != nil {
		log.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	// Local convenience: a .env beside the config file fills in anything the
	// real environment has not already set.
	if err := config.LoadEnvFile(configPath); err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// ReadWrite because the gateway inserts the turn row before signalling —
	// HTTP has to hand back a turn_id in its 202, and a workflow cannot write
	// to SQLite without breaking determinism.
	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	defer stores.Close()

	// Agent definitions live in the registry, not the config file. The config's
	// agents list is a one-time bootstrap: once the registry has any agent,
	// editing the YAML has no effect.
	reg, err := registry.Open(filepath.Join(cfg.WorkspaceRoot, "registry.db"))
	if err != nil {
		return err
	}
	defer reg.Close()

	// The gateway encrypts secrets as they are registered through the API, so it
	// needs the same master key as the worker. Without one, the secret endpoints
	// fail closed rather than storing plaintext.
	if enabled, err := reg.EnableSecretsFromEnv(cfg.Container.SecretsKeyEnv); err != nil {
		return err
	} else if enabled {
		log.Info("secret store enabled", "key_env", cfg.Container.SecretsKeyEnv)
	} else {
		log.Info("secret store disabled; master key unset", "key_env", cfg.Container.SecretsKeyEnv)
	}

	// Version snapshots capture the persona alongside the definition, and the
	// persona is a file in the agent's workspace rather than a registry column.
	// Set before seeding, so a seeded agent's first version carries the CLAUDE.md
	// that was already sitting in its workspace.
	reg.UsePersonaSource(registry.PersonaFromWorkspace(cfg.WorkDir))

	// Likewise for what a tool or skill is made of: the registry stores the
	// witness and this supplies the reading, so it never learns the host layout.
	reg.UseIdentitySource(registry.IdentityByReading())

	seeded, err := reg.Seed(context.Background(), configAgents(cfg))
	if err != nil {
		return err
	}
	if seeded > 0 {
		log.Info("seeded the agent registry from the config file; the YAML agents list is now ignored",
			"agents", seeded)
	}

	// Agents that predate version history get a version 1 describing what they
	// are now, so their next edit is recorded as a change to something rather
	// than as the beginning of everything.
	if backfilled, err := reg.BackfillVersions(context.Background()); err != nil {
		return err
	} else if backfilled > 0 {
		log.Info("recorded a first version for agents that had no history", "agents", backfilled)
	}

	// Tools and skills registered before they had history get the same treatment.
	// They declared no identity, so their first version records none — which is
	// the honest reading, not a gap to be filled by guessing at host_path.
	if backfilled, err := reg.BackfillToolVersions(context.Background()); err != nil {
		return err
	} else if backfilled > 0 {
		log.Info("recorded a first version for tools that had no history", "tools", backfilled)
	}
	if backfilled, err := reg.BackfillSkillVersions(context.Background()); err != nil {
		return err
	} else if backfilled > 0 {
		log.Info("recorded a first version for skills that had no history", "skills", backfilled)
	}

	tc, err := dialTemporal(cfg, log)
	if err != nil {
		return err
	}
	defer tc.Close()

	disp := adapter.NewDispatcher(cfg, tc, stores, reg)
	disp.SetSchedules(scheduleBackend{tc.ScheduleClient()})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Speaking outside a turn needs a live connection, which only exists for a
	// chat tool that is configured. Hoisted out of the blocks below so the HTTP
	// API can be given each one it has; a tool with no entry is one that
	// endpoint reports rather than silently swallowing.
	senders := map[core.OriginType]adapter.MessageSender{}

	// Built once and shared: both chat edges route unbound channels the same
	// way, and two router instances would mean two credentials resolved and two
	// containers per decision.
	var router *claude.Router
	if cfg.Router.Enabled {
		cred, err := cfg.Container.ResolveCredential(os.LookupEnv)
		if err != nil {
			return fmt.Errorf("router.enabled is set but %w", err)
		}
		router = &claude.Router{
			Runtime:         cfg.Container.Runtime,
			Image:           cfg.Container.Image,
			Model:           cfg.Router.Model,
			Timeout:         cfg.Router.Timeout,
			CredentialEnv:   cred.EnvName,
			CredentialValue: cred.Value,
			// --bare is faster but reads only an API key, so use it only when
			// that is what was resolved. A setup-token runs the full CLI.
			Bare: cfg.Container.IsAPIKey(cred),
		}
		log.Info("routing enabled for unbound channels",
			"model", cfg.Router.Model, "channels", len(cfg.Router.Channels))
	}

	if token := os.Getenv(cfg.Discord.TokenEnv); token != "" {
		dc, err := adapter.NewDiscord(token, cfg.Discord.GuildID, disp, log)
		if err != nil {
			return err
		}
		if router != nil {
			dc.SetRouter(router)
		}
		// Natural-language admin is no longer the stateless /admin planner. It is an
		// ordinary agent (id "admin") given a full-scope API token and the roundclaw
		// CLI as a tool, so it manages the fleet by driving the API itself — with a
		// real session, tools, and multi-step reasoning the fixed action set could
		// not do. Nothing to wire here; it is just an agent bound to a private
		// channel. See docs/usage.md ("Agent-based admin").
		if err := dc.Start(ctx); err != nil {
			return err
		}
		defer dc.Close()
		senders[core.OriginDiscord] = dc.Sender()
	} else {
		log.Info("discord token is unset; not connecting to discord",
			"env", cfg.Discord.TokenEnv)
	}

	// Slack, over Socket Mode. Both tokens are required: the bot token
	// authorises the API calls and the app-level token opens the socket, so a
	// deployment with only one of them is told rather than left half-connected.
	// See adr/001-slack-socket-mode.
	slackBot := os.Getenv(cfg.Slack.TokenEnv)
	slackApp := os.Getenv(cfg.Slack.AppTokenEnv)
	switch {
	case slackBot != "" && slackApp != "":
		sc, err := adapter.NewSlack(slackBot, slackApp, disp, log)
		if err != nil {
			return err
		}
		if router != nil {
			sc.SetRouter(router)
		}
		if err := sc.Start(ctx); err != nil {
			return err
		}
		defer sc.Close()
		senders[core.OriginSlack] = sc.Sender()
	case slackBot != "" || slackApp != "":
		return fmt.Errorf("slack needs both tokens: %s and %s, and only one is set",
			cfg.Slack.TokenEnv, cfg.Slack.AppTokenEnv)
	default:
		log.Info("slack tokens are unset; not connecting to slack",
			"bot_env", cfg.Slack.TokenEnv, "app_env", cfg.Slack.AppTokenEnv)
	}

	if len(senders) == 0 {
		log.Warn("no chat tool is connected; running with the HTTP API only")
	}

	tokens := adapter.TokensFromEnv(os.Getenv(cfg.HTTP.TokensEnv))
	if len(tokens) == 0 {
		// Better to say so loudly at startup than to have every API call come
		// back 503 with no explanation.
		log.Warn("no API tokens configured; the HTTP API will reject every request",
			"env", cfg.HTTP.TokensEnv)
	}
	delegateTokens := adapter.TokensFromEnv(os.Getenv(cfg.HTTP.DelegateTokensEnv))
	if len(delegateTokens) > 0 {
		log.Info("delegate-scoped API tokens configured; agents can delegate to each other",
			"count", len(delegateTokens))
	}

	api := adapter.NewHTTP(disp, log, tokens, delegateTokens, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	for originType, s := range senders {
		api.SetMessageSender(originType, s)
	}
	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived and a write deadline
		// would sever them mid-turn.
		IdleTimeout: 2 * time.Minute,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http api listening", "addr", cfg.HTTP.Addr, "agents", len(cfg.Agents))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// configAgents converts the config file's agent list into registry records for
// the one-time bootstrap.
func configAgents(cfg *config.Config) []registry.Agent {
	out := make([]registry.Agent, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		out = append(out, registry.Agent{
			ID:              a.ID,
			Description:     a.Description,
			AgentName:       a.AgentName,
			PermissionMode:  a.PermissionMode,
			AllowedTools:    a.AllowedTools,
			AdditionalDirs:  a.AdditionalDirs,
			DiscordChannels: a.DiscordChannels,
			Enabled:         true,
		})
	}
	return out
}

// dialTemporal connects, retrying while the server is still coming up.
//
// Under compose the worker, the gateway and Temporal start together, and
// Temporal takes the longest. Without this, both binaries would exit on the
// first refused connection and rely on the restart policy to paper over it —
// which works, but fills the logs with crashes every boot and makes a real
// outage hard to spot.
func dialTemporal(cfg *config.Config, log *slog.Logger) (client.Client, error) {
	const (
		attempts = 60
		wait     = 2 * time.Second
	)
	var lastErr error
	for i := range attempts {
		tc, err := client.Dial(client.Options{
			HostPort:  cfg.Temporal.HostPort,
			Namespace: cfg.Temporal.Namespace,
			Logger:    log,
		})
		if err == nil {
			return tc, nil
		}
		lastErr = err
		if i == 0 {
			log.Info("waiting for temporal", "host_port", cfg.Temporal.HostPort)
		}
		time.Sleep(wait)
	}
	return nil, fmt.Errorf("temporal at %s did not become reachable: %w", cfg.Temporal.HostPort, lastErr)
}

// scheduleBackend adapts Temporal's schedule client to the narrow interface the
// dispatcher uses. The SDK addresses a schedule through a handle rather than by
// ID on every call, so each method fetches one; that is a local object, not a
// round trip.
type scheduleBackend struct{ sc client.ScheduleClient }

func (b scheduleBackend) Create(ctx context.Context, opts client.ScheduleOptions) error {
	_, err := b.sc.Create(ctx, opts)
	return err
}

func (b scheduleBackend) Delete(ctx context.Context, id string) error {
	return b.sc.GetHandle(ctx, id).Delete(ctx)
}

func (b scheduleBackend) Pause(ctx context.Context, id, note string) error {
	return b.sc.GetHandle(ctx, id).Pause(ctx, client.SchedulePauseOptions{Note: note})
}

func (b scheduleBackend) Unpause(ctx context.Context, id, note string) error {
	return b.sc.GetHandle(ctx, id).Unpause(ctx, client.ScheduleUnpauseOptions{Note: note})
}

func (b scheduleBackend) Describe(ctx context.Context, id string) (*client.ScheduleDescription, error) {
	return b.sc.GetHandle(ctx, id).Describe(ctx)
}
