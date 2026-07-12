// Package server manages the home server's persistent identity and
// capability advertisement.
//
// Phase 1 — Server Identity:
// Each home server has a stable, unique identity generated on first
// boot and persisted across restarts. The identity consists of:
//   - server_id:    a UUIDv4 (human-readable form, e.g.
//                   "550e8400-e29b-41d4-a716-446655440000")
//   - name:         a human-friendly name (configurable, defaults to
//                   the hostname)
//   - public_key:   an Ed25519 public key (hex-encoded). The
//                   corresponding private key never leaves the
//                   server. Future clients can use it to verify
//                   signed challenges during device binding, and
//                   operators can pin it to detect MITM.
//   - version:      the home-datacenter-api build version
//   - capabilities: a string list advertising which features this
//                   server supports (e.g. "ipv6", "p2p", "camera",
//                   "frigate"). Used by future clients to decide
//                   which APIs to call.
//   - created_at:   the timestamp of the first boot
//
// The identity is stored in the `server_identity` SQLite table as a
// singleton row (id=1). On subsequent boots the existing row is
// loaded — the key material is never regenerated.
package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Identity is the persistent, unique identity of this home server.
//
// JSON tags produce the shape returned by GET /api/v1/server/info.
// The PrivateKey field uses `json:"-"` so the secret never leaks
// through any API response or log line that marshals the struct.
type Identity struct {
	ID         uint      `json:"-"`          // DB primary key (always 1)
	ServerID   string    `json:"server_id"`  // UUIDv4
	Name       string    `json:"name"`       // human-friendly name
	PublicKey  string    `json:"public_key"` // Ed25519 public key, hex
	PrivateKey string    `json:"-"`          // Ed25519 private key, hex (never serialized)
	Version    string    `json:"version"`    // build version
	CreatedAt  time.Time `json:"created_at"` // first-boot timestamp
}

// Capability is a feature flag advertised in GET /server/info.
type Capability string

const (
	CapIPv6    Capability = "ipv6"    // server has public IPv6
	CapP2P     Capability = "p2p"     // server supports P2P hole punching
	CapRelay   Capability = "relay"   // server has a relay (Cloudflare Tunnel)
	CapCamera  Capability = "camera"  // camera platformization (internal/camera)
	CapFrigate Capability = "frigate" // Frigate AI detection / recording
	CapMQTT    Capability = "mqtt"    // MQTT broker reachable
	CapWS      Capability = "ws"      // WebSocket push channel
)

// AllCapabilities returns the full list of capabilities this server
// instance can advertise. The list is computed at boot from the
// actually wired subsystems — so a dev build without P2P (p2p_port=0)
// correctly omits "p2p" from the advertisement.
//
// The caller passes in flags indicating which subsystems are live;
// the function returns the canonical ordered list.
func AllCapabilities(
	hasIPv6, hasP2P, hasRelay, hasCamera, hasFrigate, hasMQTT, hasWS bool,
) []Capability {
	caps := make([]Capability, 0, 7)
	if hasIPv6 {
		caps = append(caps, CapIPv6)
	}
	if hasP2P {
		caps = append(caps, CapP2P)
	}
	if hasRelay {
		caps = append(caps, CapRelay)
	}
	if hasCamera {
		caps = append(caps, CapCamera)
	}
	if hasFrigate {
		caps = append(caps, CapFrigate)
	}
	if hasMQTT {
		caps = append(caps, CapMQTT)
	}
	if hasWS {
		caps = append(caps, CapWS)
	}
	return caps
}

// LoadOrCreateIdentity fetches the singleton identity row from the
// database, generating and persisting a new one on first boot.
//
//   - db:      the sqlite database handle (already migrated)
//   - name:    human-friendly server name (empty = "Home Server")
//   - version: build version string (empty = "unknown")
//
// The function is idempotent: calling it again on an already-
// initialized database returns the existing identity unchanged.
// This is critical because the private key must never be rotated
// by accident — a re-generated key would invalidate every signed
// challenge previously issued to clients.
func LoadOrCreateIdentity(
	db *sql.DB,
	name, version string,
) (*Identity, error) {
	if existing, err := loadIdentity(db); err == nil {
		// Already initialized — honor the persisted values, but
		// bump the version field if the binary was upgraded.
		if version != "" && existing.Version != version {
			existing.Version = version
			if err := updateVersion(db, version); err != nil {
				return nil, fmt.Errorf("server_identity: update version: %w", err)
			}
		}
		// Honor a config-driven name override without touching the
		// key material. Empty name = keep the persisted value.
		if name != "" && existing.Name != name {
			existing.Name = name
			if err := updateName(db, name); err != nil {
				return nil, fmt.Errorf("server_identity: update name: %w", err)
			}
		}
		return existing, nil
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("server_identity: load: %w", err)
	}

	// First boot — generate a fresh identity.
	ident, err := generateIdentity(name, version)
	if err != nil {
		return nil, err
	}
	if err := insertIdentity(db, ident); err != nil {
		return nil, fmt.Errorf("server_identity: insert: %w", err)
	}
	return ident, nil
}

// loadIdentity reads the singleton row (id=1) from server_identity.
// Returns sql.ErrNoRows if the table is empty (first boot).
func loadIdentity(db *sql.DB) (*Identity, error) {
	const q = `SELECT id, server_id, name, public_key, private_key, version, created_at
	           FROM server_identity WHERE id = 1`
	var ident Identity
	var createdAt string
	err := db.QueryRow(q).Scan(
		&ident.ID,
		&ident.ServerID,
		&ident.Name,
		&ident.PublicKey,
		&ident.PrivateKey,
		&ident.Version,
		&createdAt,
	)
	if err != nil {
		return nil, err
	}
	// SQLite stores timestamps as RFC3339 strings when using the
	// pure-Go driver. Parse once on load so the API layer can
	// re-format freely without re-parsing per request.
	t, err := time.Parse("2006-01-02 15:04:05", createdAt)
	if err != nil {
		// Fall back to RFC3339 (the format used by some SQLite builds).
		t, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("server_identity: parse created_at %q: %w", createdAt, err)
		}
	}
	ident.CreatedAt = t
	return &ident, nil
}

// generateIdentity mints a fresh Ed25519 keypair and UUIDv4 server_id.
func generateIdentity(name, version string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("server_identity: ed25519 keygen: %w", err)
	}
	if name == "" {
		name = "Home Server"
	}
	if version == "" {
		version = "unknown"
	}
	return &Identity{
		ID:         1,
		ServerID:   uuid.NewString(),
		Name:       name,
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Version:    version,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// insertIdentity persists a new identity row. The id is hard-coded
// to 1 so there is exactly one server identity per database.
func insertIdentity(db *sql.DB, ident *Identity) error {
	const q = `INSERT INTO server_identity
	           (id, server_id, name, public_key, private_key, version, created_at)
	           VALUES (1, ?, ?, ?, ?, ?, ?)`
	_, err := db.Exec(
		q,
		ident.ServerID,
		ident.Name,
		ident.PublicKey,
		ident.PrivateKey,
		ident.Version,
		ident.CreatedAt.Format("2006-01-02 15:04:05"),
	)
	return err
}

// updateVersion updates only the version column (used on binary
// upgrade — see LoadOrCreateIdentity).
func updateVersion(db *sql.DB, version string) error {
	_, err := db.Exec(`UPDATE server_identity SET version = ? WHERE id = 1`, version)
	return err
}

// updateName updates only the name column (used when the operator
// changes the configured server name).
func updateName(db *sql.DB, name string) error {
	_, err := db.Exec(`UPDATE server_identity SET name = ? WHERE id = 1`, name)
	return err
}

// MigrateSchema creates the server_identity table if it does not
// already exist. Safe to call on every boot.
func MigrateSchema(db *sql.DB) error {
	const q = `CREATE TABLE IF NOT EXISTS server_identity (
		id          INTEGER PRIMARY KEY,
		server_id   TEXT    NOT NULL UNIQUE,
		name        TEXT    NOT NULL,
		public_key  TEXT    NOT NULL,
		private_key TEXT    NOT NULL,
		version     TEXT    NOT NULL,
		created_at  TEXT    NOT NULL
	)`
	_, err := db.Exec(q)
	return err
}
