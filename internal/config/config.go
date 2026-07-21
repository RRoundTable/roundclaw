// Package config loads roundclaw's static configuration: which agents exist,
// which Discord channels map to them, and how to reach Temporal and the
// container runtime. Agent definitions are static in v1; runtime CRUD is a v2
// concern owned by the main agent.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/roundtable/roundclaw/internal/core"
)

// Config is the whole file. Secrets are never stored here — every secret is
// named by the environment variable that holds it.
type Config struct {
	// WorkspaceRoot contains one directory per agent. Relative paths resolve
	// against the config file's directory, not the process CWD, so the worker
	// and gateway agree no matter where they are started from.
	WorkspaceRoot string `yaml:"workspace_root"`

	Temporal  TemporalConfig  `yaml:"temporal"`
	Container ContainerConfig `yaml:"container"`
	Discord   DiscordConfig   `yaml:"discord"`
	HTTP      HTTPConfig      `yaml:"http"`
	Router    RouterConfig    `yaml:"router"`
	Limits    LimitsConfig    `yaml:"limits"`
	Retention RetentionConfig `yaml:"retention"`

	Agents []AgentConfig `yaml:"agents"`

	// byChannel indexes Agents by Discord channel ID; built by Load.
	byChannel map[string]*AgentConfig
	byID      map[string]*AgentConfig
}

type TemporalConfig struct {
	HostPort  string `yaml:"host_port"`
	Namespace string `yaml:"namespace"`
	TaskQueue string `yaml:"task_queue"`
}

type ContainerConfig struct {
	// Runtime is the CLI to invoke ("docker", "podman", ...).
	Runtime string `yaml:"runtime"`
	// Image must contain the `claude` binary. roundclaw ships no code into it.
	Image string `yaml:"image"`
	// APIKeyEnv names the host env var holding an Anthropic API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// OAuthTokenEnv names the host env var holding a long-lived Claude Code
	// token from `claude setup-token`. When both are set this one wins.
	//
	// Reusing ~/.claude/.credentials.json instead would be a mistake: those
	// credentials belong to an interactive session, and a container refreshing
	// them can rotate the refresh token out from under the human still using
	// it. `claude setup-token` exists precisely to avoid that.
	OAuthTokenEnv string `yaml:"oauth_token_env"`
	// SecretsKeyEnv names the host env var holding the master key that encrypts
	// registered agent secrets at rest. Empty (the default env var unset)
	// disables the secret store: agents that use no secrets are unaffected, and
	// any attempt to register one fails closed rather than storing plaintext.
	SecretsKeyEnv string `yaml:"secrets_key_env"`
	// TurnTimeout bounds a single agent turn end to end.
	TurnTimeout time.Duration `yaml:"turn_timeout"`
	// StopGrace is how long SIGINT gets before SIGKILL when a turn is cancelled.
	StopGrace time.Duration `yaml:"stop_grace"`
}

type DiscordConfig struct {
	TokenEnv string `yaml:"token_env"`
	// GuildID scopes slash-command registration. Empty registers globally,
	// which Discord propagates slowly; set it during development.
	GuildID string `yaml:"guild_id"`

	// CommandPermission decides who can see and run roundclaw's slash commands.
	//
	// Discord enforces this itself, before the interaction ever reaches us,
	// which makes it the cheapest guard available against an ordinary member
	// stopping someone else's work or spending tokens. It defaults to the
	// restrictive option because the commands are destructive and billable.
	//
	// One of: manage_guild (default), administrator, everyone.
	CommandPermission string `yaml:"command_permission"`

	// AllowedRoles restricts who may run roundclaw's commands, by Discord role
	// ID. Empty means anyone Discord's own permission gate lets through.
	//
	// This is checked inside roundclaw rather than left to Discord because
	// Discord's gate is coarse: it can say "Manage Server" but not "the three
	// people who are allowed to spend tokens". Both apply — Discord filters
	// first, this filters second.
	AllowedRoles []string `yaml:"allowed_roles"`

	// AllowedUsers restricts by user ID, for people who should have access
	// without being given a role.
	AllowedUsers []string `yaml:"allowed_users"`

	// ReadOnlyCommands are exempt from the role check. Asking what an agent is
	// doing costs nothing and helps whoever is confused about why it is busy;
	// gating it makes the bot feel broken rather than secure.
	readOnly map[string]bool
}

// CommandsRestricted reports whether an allow-list is configured at all.
func (d DiscordConfig) CommandsRestricted() bool {
	return len(d.AllowedRoles) > 0 || len(d.AllowedUsers) > 0
}

// PermitsCommand reports whether a caller may run a command.
//
// Read-only commands are always allowed: they cost nothing, and refusing to say
// what an agent is doing makes the bot look broken to the person best placed to
// notice something is wrong.
func (d DiscordConfig) PermitsCommand(command, userID string, roleIDs []string) bool {
	if !d.CommandsRestricted() {
		return true
	}
	if d.readOnly[command] {
		return true
	}
	for _, u := range d.AllowedUsers {
		if u == userID {
			return true
		}
	}
	allowed := make(map[string]bool, len(d.AllowedRoles))
	for _, r := range d.AllowedRoles {
		allowed[r] = true
	}
	for _, r := range roleIDs {
		if allowed[r] {
			return true
		}
	}
	return false
}

// Discord permission bits used for command gating.
const (
	permManageGuild   int64 = 1 << 5
	permAdministrator int64 = 1 << 3
)

// CommandPermissionBits renders CommandPermission for Discord.
//
// A nil result means "no restriction". An unrecognised value falls back to the
// restrictive default rather than silently opening the commands up — failing
// closed is the only safe direction for a permission setting.
func (d DiscordConfig) CommandPermissionBits() *int64 {
	switch d.CommandPermission {
	case "everyone":
		return nil
	case "administrator":
		bits := permAdministrator
		return &bits
	default:
		bits := permManageGuild
		return &bits
	}
}

type HTTPConfig struct {
	Addr string `yaml:"addr"`
	// TokensEnv names an env var holding comma-separated bearer tokens.
	TokensEnv string `yaml:"tokens_env"`
	// WaitTimeout bounds ?wait=true before the request is demoted to 202.
	WaitTimeout time.Duration `yaml:"wait_timeout"`
	// MaxSSEPerAgent caps concurrent SSE streams so the gateway cannot run out
	// of file descriptors serving status watchers.
	MaxSSEPerAgent int `yaml:"max_sse_per_agent"`
	// CallbackSecretEnv names an env var holding the HMAC key used to sign
	// outbound callback deliveries.
	CallbackSecretEnv string `yaml:"callback_secret_env"`
	// WebhookSecretEnv names an env var holding the shared secret inbound
	// webhooks sign their payloads with. Empty refuses every webhook, because
	// without it any caller on the network could queue billable work.
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// RouterConfig controls what happens to a plain message in a channel bound to
// no agent.
//
// It is off by default, and deliberately so: turning it on means every such
// message costs an LLM call. Explicit `/ask` covers the same ground for free,
// so routing is an ergonomic upgrade, not a requirement.
type RouterConfig struct {
	Enabled bool `yaml:"enabled"`
	// Model for the routing call. A small fast model is the point — routing is
	// a classification, not the work itself.
	Model   string        `yaml:"model"`
	Timeout time.Duration `yaml:"timeout"`
	// Channels limits routing to these channel IDs. Empty means every unbound
	// channel the bot can see, which on a busy server is a large bill.
	Channels []string `yaml:"channels"`

	channelSet map[string]bool
}

// RoutesChannel reports whether routing applies to a channel.
func (r RouterConfig) RoutesChannel(channelID string) bool {
	if !r.Enabled {
		return false
	}
	if len(r.channelSet) == 0 {
		return true
	}
	return r.channelSet[channelID]
}

// LimitsConfig bounds what roundclaw is allowed to spend.
//
// Every agent turn costs real money, and the paths that start one — a Discord
// command, an API call, a schedule — are all cheap to trigger. Without a
// ceiling the only feedback is the bill.
//
// A zero value means unlimited for that dimension, so an operator opts into
// each ceiling deliberately rather than discovering an invented default.
type LimitsConfig struct {
	// TurnsPerHour caps how often a single agent can be asked to run.
	TurnsPerHour int `yaml:"turns_per_hour"`
	// CostPerDayUSD caps one agent's daily spend.
	CostPerDayUSD float64 `yaml:"cost_per_day_usd"`
	// GlobalCostPerDayUSD caps everything together. This is the backstop: a
	// per-agent limit does nothing about twenty agents each staying just under
	// theirs.
	GlobalCostPerDayUSD float64 `yaml:"global_cost_per_day_usd"`
	// MaxConcurrentTurns caps containers running at once across all agents.
	// Applied to the Temporal worker, so excess turns wait rather than fail.
	MaxConcurrentTurns int `yaml:"max_concurrent_turns"`
}

// RetentionConfig bounds how much history each agent keeps.
//
// live_logs grows by a row per stream-json event per turn and had no delete
// path at all, so a busy agent's database grew without limit. Transcripts are
// pruned harder than turns: a turn row is the small, durable audit trail, while
// its transcript is bulky and only interesting while the work is recent.
type RetentionConfig struct {
	// TranscriptDays keeps live_logs for finished turns this long.
	TranscriptDays int `yaml:"transcript_days"`
	// TurnDays keeps turn rows this long. Should exceed TranscriptDays.
	TurnDays int `yaml:"turn_days"`
	// Interval is how often the sweep runs.
	Interval time.Duration `yaml:"interval"`
}

// Enabled reports whether pruning is configured. Zero means keep everything,
// so deleting history is always something an operator asked for.
func (r RetentionConfig) Enabled() bool {
	return r.TranscriptDays > 0 || r.TurnDays > 0
}

// AgentConfig is one persistent agent: an identity, a Claude Code agent
// definition to run as, and the channels bound to it.
type AgentConfig struct {
	ID string `yaml:"id"`
	// Description is shown in /agents and in slash-command autocomplete, so it
	// should say what the agent is for in a few words.
	Description string `yaml:"description"`
	// AgentName is passed to `claude --agent`. Empty runs the default Claude
	// Code system prompt instead of a named subagent definition.
	AgentName string `yaml:"agent_name"`

	PermissionMode string   `yaml:"permission_mode"`
	AllowedTools   []string `yaml:"allowed_tools"`
	// AdditionalDirs are mounted read-only and passed via --add-dir.
	AdditionalDirs []string `yaml:"additional_dirs"`

	DiscordChannels []string `yaml:"discord_channels"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}
	c.applyDefaults()
	if !filepath.IsAbs(c.WorkspaceRoot) {
		c.WorkspaceRoot = filepath.Join(base, c.WorkspaceRoot)
	}

	if err := c.index(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = "workspace"
	}
	if c.Temporal.HostPort == "" {
		c.Temporal.HostPort = "localhost:7233"
	}
	if c.Temporal.Namespace == "" {
		c.Temporal.Namespace = "default"
	}
	if c.Temporal.TaskQueue == "" {
		c.Temporal.TaskQueue = "roundclaw"
	}
	if c.Container.Runtime == "" {
		c.Container.Runtime = "docker"
	}
	if c.Container.APIKeyEnv == "" {
		c.Container.APIKeyEnv = "ANTHROPIC_API_KEY"
	}
	if c.Container.OAuthTokenEnv == "" {
		c.Container.OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	}
	if c.Container.SecretsKeyEnv == "" {
		c.Container.SecretsKeyEnv = "ROUNDCLAW_SECRET_KEY"
	}
	if c.Container.TurnTimeout == 0 {
		c.Container.TurnTimeout = 30 * time.Minute
	}
	if c.Container.StopGrace == 0 {
		c.Container.StopGrace = 10 * time.Second
	}
	if c.Discord.TokenEnv == "" {
		c.Discord.TokenEnv = "DISCORD_TOKEN"
	}
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = ":8080"
	}
	if c.HTTP.TokensEnv == "" {
		c.HTTP.TokensEnv = "ROUNDCLAW_API_TOKENS"
	}
	if c.HTTP.WaitTimeout == 0 {
		c.HTTP.WaitTimeout = 60 * time.Second
	}
	if c.HTTP.MaxSSEPerAgent == 0 {
		c.HTTP.MaxSSEPerAgent = 8
	}
	if c.HTTP.CallbackSecretEnv == "" {
		c.HTTP.CallbackSecretEnv = "ROUNDCLAW_CALLBACK_SECRET"
	}
	if c.HTTP.WebhookSecretEnv == "" {
		c.HTTP.WebhookSecretEnv = "ROUNDCLAW_WEBHOOK_SECRET"
	}
	if c.Router.Timeout == 0 {
		c.Router.Timeout = 30 * time.Second
	}
	if c.Router.Model == "" {
		c.Router.Model = "haiku"
	}
	// Commands that only read. Everything else can spend money or destroy work
	// in progress, so it needs the allow-list when one is configured.
	c.Discord.readOnly = map[string]bool{
		"agents": true, "status": true, "workflow": true,
	}
	if c.Retention.Interval == 0 {
		c.Retention.Interval = 6 * time.Hour
	}
	if c.Limits.MaxConcurrentTurns == 0 {
		// Unlike the spend ceilings, this one gets a default: it bounds host
		// resources rather than money, and an unbounded worker will happily
		// start a container per queued turn.
		c.Limits.MaxConcurrentTurns = 5
	}
	c.Router.channelSet = make(map[string]bool, len(c.Router.Channels))
	for _, ch := range c.Router.Channels {
		c.Router.channelSet[ch] = true
	}
}

func (c *Config) index() error {
	if c.Container.Image == "" {
		return fmt.Errorf("config: container.image is required")
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("config: at least one agent must be defined")
	}

	c.byChannel = make(map[string]*AgentConfig, len(c.Agents))
	c.byID = make(map[string]*AgentConfig, len(c.Agents))

	for i := range c.Agents {
		a := &c.Agents[i]
		if err := core.ValidateAgentID(a.ID); err != nil {
			return fmt.Errorf("config: agent[%d]: %w", i, err)
		}
		if _, dup := c.byID[a.ID]; dup {
			return fmt.Errorf("config: duplicate agent id %q", a.ID)
		}
		c.byID[a.ID] = a

		for _, ch := range a.DiscordChannels {
			if prev, dup := c.byChannel[ch]; dup {
				// Two agents on one channel would make routing ambiguous and
				// silently send a reply to the wrong session.
				return fmt.Errorf("config: discord channel %s is bound to both %q and %q", ch, prev.ID, a.ID)
			}
			c.byChannel[ch] = a
		}
	}
	return nil
}

// Credential is the Anthropic credential to inject into a container: the name
// of the environment variable to set inside it, and the value taken from the
// host's environment.
type Credential struct {
	EnvName string
	Value   string
}

// ResolveCredential reads whichever credential the host environment provides.
//
// A long-lived OAuth token from `claude setup-token` wins over an API key when
// both are present, because it is the one meant for headless use. lookup is
// normally os.LookupEnv.
func (c *ContainerConfig) ResolveCredential(lookup func(string) (string, bool)) (Credential, error) {
	for _, name := range []string{c.OAuthTokenEnv, c.APIKeyEnv} {
		if name == "" {
			continue
		}
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			return Credential{EnvName: name, Value: v}, nil
		}
	}
	return Credential{}, fmt.Errorf(
		"no Anthropic credential: set %s (from `claude setup-token`) or %s",
		c.OAuthTokenEnv, c.APIKeyEnv)
}

// IsAPIKey reports whether a resolved credential is the API key rather than an
// OAuth setup-token. The router uses this to decide whether it can run --bare,
// which is faster but reads only an API key.
func (c *ContainerConfig) IsAPIKey(cred Credential) bool {
	return c.APIKeyEnv != "" && cred.EnvName == c.APIKeyEnv
}

// AgentByID returns the agent with the given ID, or nil.
func (c *Config) AgentByID(id string) *AgentConfig { return c.byID[id] }

// AllAgents returns every configured agent in file order. Used by /agents and
// by slash-command autocomplete, so the ordering is the operator's, not a map's.
func (c *Config) AllAgents() []AgentConfig { return c.Agents }

// Label renders an agent for a picker: its ID plus a short description.
func (a AgentConfig) Label() string {
	if a.Description == "" {
		return a.ID
	}
	label := a.ID + " — " + a.Description
	// Discord rejects choice names longer than 100 characters.
	if len(label) > 100 {
		label = label[:97] + "..."
	}
	return label
}

// AgentByChannel returns the agent bound to a Discord channel, or nil when the
// channel is unbound. v1 ignores unbound channels; v2 routes them to the main
// agent instead.
func (c *Config) AgentByChannel(channelID string) *AgentConfig { return c.byChannel[channelID] }

// AgentDir is an agent's private directory tree under the workspace root.
func (c *Config) AgentDir(agentID string) string {
	return filepath.Join(c.WorkspaceRoot, agentID)
}

// WorkDir is mounted into the container at /workspace.
func (c *Config) WorkDir(agentID string) string {
	return filepath.Join(c.AgentDir(agentID), "work")
}

// ClaudeHomeDir is mounted at the container's ~/.claude. It must survive
// container removal or `claude --resume` cannot find the session transcript.
func (c *Config) ClaudeHomeDir(agentID string) string {
	return filepath.Join(c.AgentDir(agentID), "claude-home")
}

// DBPath is the agent's private SQLite file. One file per agent keeps writers
// from contending on a single database lock.
func (c *Config) DBPath(agentID string) string {
	return filepath.Join(c.AgentDir(agentID), "state.db")
}
