// Package core holds the types shared by every inbound adapter, the Temporal
// workflow, and every outbound adapter. Nothing here may import an adapter or a
// Temporal package: adapters depend on core, never the reverse. That is what
// keeps "add an event source" from turning into a core change.
package core

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// OriginType tags an Origin. Response delivery branches on this and nothing
// else, so a new event source needs a constant here plus one case in
// DeliverResponse.
type OriginType string

const (
	// OriginDiscord routes the result back to the channel it came from.
	OriginDiscord OriginType = "discord"
	// OriginHTTPPoll records the result in SQLite and stops there; the client
	// collects it via GET /v1/turns/{id} or the SSE stream.
	OriginHTTPPoll OriginType = "http_poll"
	// OriginHTTPCallback POSTs the result to a caller-supplied URL.
	OriginHTTPCallback OriginType = "http_callback"
	// OriginAgent hands the result to another agent as a new request, in a named
	// conversation of that agent. It is how a delegated turn reports back: the
	// delegator wakes with the result and answers the human in its own words.
	//
	// Unlike the other three this origin creates work rather than just
	// delivering, which is why admission has to bound it (see the depth and
	// budget checks at the dispatcher) and why an agent may not name itself in
	// its own conversation — that would be an unbounded self-loop.
	OriginAgent OriginType = "agent"
)

// Origin says where a turn's response has to go. It is persisted as JSON in
// turns.origin and is the single source of truth for routing — the workflow
// never carries a "reply to" closure, because closures do not survive a worker
// restart and this does.
type Origin struct {
	Type OriginType `json:"type"`

	// Discord
	ChannelID string `json:"channel_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`

	// HTTP callback. SecretRef names an HMAC key in config rather than holding
	// the key itself, so secrets stay out of the Temporal history and the DB.
	URL       string `json:"url,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`

	// Agent. Conversation may be empty, meaning that agent's default
	// conversation. The reply address the woken agent then answers to is not
	// stored here: it is read from the last human-facing turn of that
	// conversation, which is by definition where the conversation answers.
	Agent        string `json:"agent,omitempty"`
	Conversation string `json:"conversation,omitempty"`
}

// DiscordOrigin builds an Origin that replies in a Discord channel.
func DiscordOrigin(channelID, messageID string) Origin {
	return Origin{Type: OriginDiscord, ChannelID: channelID, MessageID: messageID}
}

// HTTPPollOrigin builds an Origin for a caller that will fetch the result.
func HTTPPollOrigin() Origin {
	return Origin{Type: OriginHTTPPoll}
}

// HTTPCallbackOrigin builds an Origin that pushes the result to callbackURL.
func HTTPCallbackOrigin(callbackURL, secretRef string) Origin {
	return Origin{Type: OriginHTTPCallback, URL: callbackURL, SecretRef: secretRef}
}

// AgentOrigin builds an Origin that wakes agentID in conversationID with the
// result. Empty conversationID means that agent's default conversation.
func AgentOrigin(agentID, conversationID string) Origin {
	return Origin{Type: OriginAgent, Agent: agentID, Conversation: conversationID}
}

// Validate rejects an Origin that delivery could not act on. Callers must run
// this at the adapter boundary: an Origin that fails here would otherwise be
// discovered only after the agent turn has already burned tokens.
func (o Origin) Validate() error {
	switch o.Type {
	case OriginDiscord:
		if o.ChannelID == "" {
			return fmt.Errorf("discord origin: channel_id is required")
		}
		return nil
	case OriginHTTPPoll:
		return nil
	case OriginHTTPCallback:
		if o.URL == "" {
			return fmt.Errorf("http_callback origin: url is required")
		}
		u, err := url.Parse(o.URL)
		if err != nil {
			return fmt.Errorf("http_callback origin: url is not parseable: %w", err)
		}
		// Structural checks only. The SSRF address check lives in
		// AssertPublicCallbackHost because it performs DNS; callers must run
		// both.
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("http_callback origin: url scheme %q is not http(s)", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("http_callback origin: url has no host")
		}
		return nil
	case OriginAgent:
		if err := ValidateAgentID(o.Agent); err != nil {
			return fmt.Errorf("agent origin: %w", err)
		}
		return nil
	case "":
		return fmt.Errorf("origin: type is required")
	default:
		return fmt.Errorf("origin: unknown type %q", o.Type)
	}
}

// MarshalText renders an Origin for logs without leaking the callback URL's
// query string, which callers sometimes use to carry their own tokens.
func (o Origin) String() string {
	switch o.Type {
	case OriginDiscord:
		return "discord:" + o.ChannelID
	case OriginAgent:
		if o.Conversation == "" {
			return "agent:" + o.Agent
		}
		return "agent:" + o.Agent + "/" + o.Conversation
	case OriginHTTPCallback:
		if u, err := url.Parse(o.URL); err == nil {
			return "http_callback:" + u.Scheme + "://" + u.Host + u.Path
		}
		return "http_callback:<unparseable>"
	default:
		return string(o.Type)
	}
}

// AssertPublicCallbackHost rejects a callback target that resolves into the
// host's own network. Without it, any API caller could use roundclaw to probe
// or POST to internal services and cloud metadata endpoints.
//
// This is deliberately separate from Validate: it performs DNS, so it must not
// be hidden inside a cheap structural check. It has to run twice — once at
// admission so a caller is refused before an agent turn is paid for, and again
// at delivery, because a name can be re-pointed at a private address in
// between.
func AssertPublicCallbackHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("callback url is not parseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("callback url scheme %q is not http(s)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("callback url has no host")
	}

	// A bare IP literal is checked directly; only a name needs resolving.
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("callback host %s is not a public address", ip)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve callback host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("callback host %q resolved to no addresses", host)
	}
	// Every address must be public: one private answer is enough to make the
	// request reachable inside the network.
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("callback host %q resolves to non-public address %s", host, ip)
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate but is
	// still routable inside many hosting environments.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// EncodeOrigin serialises an Origin for the turns.origin column.
func EncodeOrigin(o Origin) (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("encode origin: %w", err)
	}
	return string(b), nil
}

// DecodeOrigin parses a turns.origin value. An unknown type is returned as-is
// rather than rejected, so that a row written by a newer binary can still be
// read and reported by an older one.
func DecodeOrigin(s string) (Origin, error) {
	var o Origin
	if strings.TrimSpace(s) == "" {
		return o, fmt.Errorf("decode origin: empty value")
	}
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return o, fmt.Errorf("decode origin: %w", err)
	}
	if o.Type == "" {
		return o, fmt.Errorf("decode origin: missing type")
	}
	return o, nil
}
