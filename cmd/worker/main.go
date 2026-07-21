// Command worker runs roundclaw's Temporal worker: it executes agent turns in
// containers and delivers the results. It holds no inbound listeners, so it can
// be scaled or restarted independently of the gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/roundtable/roundclaw/internal/adapter"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

func main() {
	configPath := flag.String("config", "roundclaw.yaml", "path to the roundclaw config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, log); err != nil {
		log.Error("worker exited", "error", err)
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

	// The worker decrypts registered secrets to inject them into agent
	// containers, so it needs the master key. Without one, the secret store stays
	// off and agents that use no secrets run exactly as before.
	if enabled, err := reg.EnableSecretsFromEnv(cfg.Container.SecretsKeyEnv); err != nil {
		return err
	} else if enabled {
		log.Info("secret store enabled", "key_env", cfg.Container.SecretsKeyEnv)
	} else {
		log.Info("secret store disabled; master key unset", "key_env", cfg.Container.SecretsKeyEnv)
	}

	seeded, err := reg.Seed(context.Background(), configAgents(cfg))
	if err != nil {
		return err
	}
	if seeded > 0 {
		log.Info("seeded the agent registry from the config file; the YAML agents list is now ignored",
			"agents", seeded)
	}

	// REST-only Discord client. The gateway owns the single websocket
	// connection; a second one here would double-consume every inbound event.
	var discord activity.DiscordSender
	if token := os.Getenv(cfg.Discord.TokenEnv); token != "" {
		sender, err := adapter.RESTSender(token)
		if err != nil {
			return err
		}
		discord = sender
	} else {
		log.Warn("discord token is unset; discord deliveries will fail",
			"env", cfg.Discord.TokenEnv)
	}

	tc, err := dialTemporal(cfg, log)
	if err != nil {
		return err
	}
	defer tc.Close()

	w := worker.New(tc, cfg.Temporal.TaskQueue, worker.Options{
		// Calling RecordHeartbeat often is not enough on its own: the SDK
		// throttles what it actually sends to roughly 80% of HeartbeatTimeout,
		// and cancellation only reaches an activity on the heartbeat response.
		// With a 30s HeartbeatTimeout that would make /stop and /steer take up
		// to ~24 seconds to reach the container. Capping the throttle decouples
		// "how often we report" from "how long before we're declared dead".
		MaxHeartbeatThrottleInterval: time.Second,
		// Bounds containers running at once. Excess turns wait in Temporal
		// rather than being refused, which is the right shape for a resource
		// ceiling as opposed to a spend ceiling.
		MaxConcurrentActivityExecutionSize: cfg.Limits.MaxConcurrentTurns,
	})
	w.RegisterWorkflow(rcworkflow.SubAgent)
	// Started by a Temporal schedule; it only queues a request and finishes.
	w.RegisterWorkflow(rcworkflow.ScheduledRequest)
	// Registering the struct exposes every exported method as an activity,
	// which is why Activities carries no exported non-activity methods.
	w.RegisterActivity(activity.NewActivities(cfg, stores, reg, discord, tc))

	// Cancelled when the worker stops, so a sweep cannot outlive the process.
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	defer stopRetention()
	go runRetention(retentionCtx, cfg, reg, stores, log)

	log.Info("worker starting",
		"task_queue", cfg.Temporal.TaskQueue,
		"agents", len(cfg.Agents),
		"image", cfg.Container.Image)

	// InterruptCh makes the worker drain in-flight activity tasks on SIGTERM
	// rather than abandoning a running container.
	return w.Run(worker.InterruptCh())
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
