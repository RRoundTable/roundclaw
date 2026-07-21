package registry

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Secrets an agent's container needs at runtime — a GITHUB_TOKEN for a
// pull-request reviewer, an API key for a tool it calls. Unlike the config's
// *_env names, these carry the value: it is encrypted at rest with a master key
// held only in the process environment, decrypted just long enough to hand to
// the container as an environment variable.
//
// A secret has a scope: an agent ID, or the empty string for a global secret
// every agent sees. Per-agent wins over global on a name collision, so a shared
// default can be overridden for one agent.
//
// The value is never returned by any list method and never logged. The only way
// it leaves this package is SecretsForAgent, which the activity calls to build
// the container's environment.

const secretSchema = `
CREATE TABLE IF NOT EXISTS secrets (
    scope      TEXT NOT NULL,          -- agent id, or '' for a global secret
    name       TEXT NOT NULL,          -- environment variable name
    ciphertext TEXT NOT NULL,          -- base64(nonce || AES-256-GCM ciphertext)
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, name)
);
`

// ErrSecretsDisabled is returned when a secret operation is attempted but no
// master key was configured. Failing closed matters: silently storing plaintext
// or silently dropping a secret would both be worse than an error.
var ErrSecretsDisabled = errors.New("secrets are not configured: set the master-key env var")

// SecretMeta describes a stored secret without its value.
type SecretMeta struct {
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UseSecretKey enables the secret store by deriving an AES-256 key from the
// master key. Any string works — the raw value is hashed to a 32-byte key — so
// an operator can use a passphrase, though a random value
// (`openssl rand -base64 32`) is what to want. Rotating the master key makes
// every stored secret undecryptable, which is the intended failure: a rotated
// key means the old ciphertext should no longer be readable.
//
// Called by the processes that touch secrets — the gateway to write them, the
// worker to inject them — after opening the registry. A process that never
// configures a key simply cannot use secrets, and says so rather than guessing.
func (s *Store) UseSecretKey(raw string) error {
	if raw == "" {
		return ErrSecretsDisabled
	}
	sum := sha256.Sum256([]byte(raw))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return fmt.Errorf("build secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("build secret aead: %w", err)
	}
	s.aead = aead
	return nil
}

// SecretsEnabled reports whether a master key has been configured.
func (s *Store) SecretsEnabled() bool { return s.aead != nil }

// EnableSecretsFromEnv configures the secret store from the named environment
// variable, returning whether it was enabled. An unset variable is not an error:
// it simply leaves the store off. The caller logs which case happened.
func (s *Store) EnableSecretsFromEnv(keyEnv string) (bool, error) {
	key := os.Getenv(keyEnv)
	if key == "" {
		return false, nil
	}
	if err := s.UseSecretKey(key); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) seal(value string) (string, error) {
	if s.aead == nil {
		return "", ErrSecretsDisabled
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Store) open(ciphertext string) (string, error) {
	if s.aead == nil {
		return "", ErrSecretsDisabled
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("secret ciphertext too short")
	}
	nonce, body := raw[:ns], raw[ns:]
	plain, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		// Almost always a master key that no longer matches what sealed this.
		return "", fmt.Errorf("decrypt secret: %w (wrong master key?)", err)
	}
	return string(plain), nil
}

// PutSecret stores or replaces a secret. scope is an agent ID, or "" for a
// global secret. The name must be a valid environment-variable name, since that
// is what it becomes inside the container.
func (s *Store) PutSecret(ctx context.Context, scope, name, value string) error {
	if !s.SecretsEnabled() {
		return ErrSecretsDisabled
	}
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	if scope != "" {
		if _, err := s.Get(ctx, scope); err != nil {
			return err
		}
	}
	ciphertext, err := s.seal(value)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO secrets (scope, name, ciphertext, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(scope, name) DO UPDATE SET
			ciphertext = excluded.ciphertext,
			updated_at = excluded.updated_at`,
		scope, name, ciphertext, now, now)
	if err != nil {
		return fmt.Errorf("store secret %q: %w", name, err)
	}
	return nil
}

// DeleteSecret removes one secret. Removing one that does not exist is not an
// error: the caller wanted it gone, and it is.
func (s *Store) DeleteSecret(ctx context.Context, scope, name string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM secrets WHERE scope = ? AND name = ?`, scope, name)
	if err != nil {
		return fmt.Errorf("delete secret %q: %w", name, err)
	}
	return nil
}

// ListSecrets returns the metadata of every secret in one scope, names only —
// the value is never exposed by a list. Ordered by name so output is stable.
func (s *Store) ListSecrets(ctx context.Context, scope string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, name, created_at, updated_at FROM secrets WHERE scope = ? ORDER BY name`, scope)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		var created, updated int64
		if err := rows.Scan(&m.Scope, &m.Name, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		m.CreatedAt = time.UnixMilli(created)
		m.UpdatedAt = time.UnixMilli(updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SecretsForAgent returns the decrypted secrets an agent's container should
// receive: every global secret, then the agent's own, so a per-agent secret
// overrides a global one of the same name. This is the one method that returns
// plaintext, and it exists only for the activity that builds the container env.
func (s *Store) SecretsForAgent(ctx context.Context, agentID string) (map[string]string, error) {
	if !s.SecretsEnabled() {
		// No key configured means no secrets to inject — not an error, so an
		// agent that uses none runs exactly as before.
		return nil, nil
	}
	out := map[string]string{}
	for _, scope := range []string{"", agentID} {
		rows, err := s.db.QueryContext(ctx,
			`SELECT name, ciphertext FROM secrets WHERE scope = ?`, scope)
		if err != nil {
			return nil, fmt.Errorf("load secrets for %q: %w", scope, err)
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var name, ciphertext string
				if err = rows.Scan(&name, &ciphertext); err != nil {
					return
				}
				var value string
				if value, err = s.open(ciphertext); err != nil {
					return
				}
				out[name] = value
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ValidateSecretName enforces the environment-variable naming rule, since the
// name is used verbatim as one. A bad name would either be silently dropped by
// the container runtime or, worse, break the whole env list.
func ValidateSecretName(name string) error {
	if name == "" {
		return fmt.Errorf("secret name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("secret name %q is longer than 128 characters", name)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("secret name %q is not a valid environment variable name; use [A-Za-z_][A-Za-z0-9_]*", name)
		}
	}
	return nil
}
