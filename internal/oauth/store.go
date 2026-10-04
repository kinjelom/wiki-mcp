package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

var (
	bucketGrants  = []byte("grants")
	bucketTokens  = []byte("tokens")
	bucketClients = []byte("clients")
)

// Grant is the authorization a user gave a client: their wiki identity and wiki credential (sealed). Access and
// refresh tokens point to a grant; revoking it signs the user out of that client.
type Grant struct {
	ID         string        `json:"id"`
	ClientID   string        `json:"client_id"`
	ClientName string        `json:"client_name,omitempty"`
	Identity   wiki.Identity `json:"identity"`
	// Credential is the wiki credential sealed with the encryption key.
	Credential []byte    `json:"credential"`
	Scopes     []string  `json:"scopes"`
	CreatedAt  time.Time `json:"created_at"`
	// ExpiresAt is the end of the session (oauth.session_max_age), zero for none.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// RefreshedAt is the last use of a refresh token.
	RefreshedAt time.Time `json:"refreshed_at,omitzero"`
}

const (
	kindAccess  = "access"
	kindRefresh = "refresh"
)

// tokenRecord is stored under the SHA-256 hash of a token, the token itself is never stored.
type tokenRecord struct {
	Kind      string    `json:"kind"`
	GrantID   string    `json:"grant_id"`
	ExpiresAt time.Time `json:"expires_at"`
	// RotatedAt is set when a refresh token was exchanged: a second use is a replay.
	RotatedAt time.Time `json:"rotated_at,omitzero"`
}

// Client is a client registered with dynamic client registration (RFC 7591). Clients identified by a client ID
// metadata document are not stored.
type Client struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	// SecretHash is the SHA-256 of the client secret of a confidential client, empty for a public one.
	SecretHash string    `json:"secret_hash,omitempty"`
	AuthMethod string    `json:"token_endpoint_auth_method"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

// Store keeps grants, tokens and registered clients in a bbolt file.
type Store struct {
	db *bolt.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketGrants, bucketTokens, bucketClients} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func put(tx *bolt.Tx, bucket []byte, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), b)
}

// get decodes the value of key into v and reports whether it exists.
func get(tx *bolt.Tx, bucket []byte, key string, v any) (bool, error) {
	b := tx.Bucket(bucket).Get([]byte(key))
	if b == nil {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (s *Store) PutGrant(g *Grant) error {
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bucketGrants, g.ID, g) })
}

// Grant returns the grant, nil when it does not exist.
func (s *Store) Grant(id string) (*Grant, error) {
	var g Grant
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) (err error) {
		ok, err = get(tx, bucketGrants, id, &g)
		return err
	})
	if err != nil || !ok {
		return nil, err
	}
	return &g, nil
}

// DeleteGrant revokes the grant; its tokens stop working at once (they are removed by Cleanup).
func (s *Store) DeleteGrant(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketGrants).Delete([]byte(id)) })
}

// IssueTokens stores an access and a refresh token of the grant and updates the grant.
func (s *Store) IssueTokens(g *Grant, accessHash string, access tokenRecord, refreshHash string, refresh tokenRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := put(tx, bucketGrants, g.ID, g); err != nil {
			return err
		}
		if err := put(tx, bucketTokens, accessHash, access); err != nil {
			return err
		}
		return put(tx, bucketTokens, refreshHash, refresh)
	})
}

// Token returns the token record, nil when it does not exist.
func (s *Store) Token(hash string) (*tokenRecord, error) {
	var r tokenRecord
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) (err error) {
		ok, err = get(tx, bucketTokens, hash, &r)
		return err
	})
	if err != nil || !ok {
		return nil, err
	}
	return &r, nil
}

// RotateRefresh marks a refresh token as exchanged and returns its record as it was before, nil when the token does
// not exist. Checking and marking happen in one transaction, so of two concurrent exchanges only one sees the token
// unused.
func (s *Store) RotateRefresh(hash string, now time.Time) (*tokenRecord, error) {
	var before tokenRecord
	var ok bool
	err := s.db.Update(func(tx *bolt.Tx) (err error) {
		ok, err = get(tx, bucketTokens, hash, &before)
		if err != nil || !ok || before.Kind != kindRefresh {
			return err
		}
		after := before
		if after.RotatedAt.IsZero() {
			after.RotatedAt = now
		}
		return put(tx, bucketTokens, hash, after)
	})
	if err != nil || !ok {
		return nil, err
	}
	return &before, nil
}

func (s *Store) PutClient(c *Client) error {
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bucketClients, c.ClientID, c) })
}

// Client returns the registered client, nil when it does not exist.
func (s *Store) Client(id string) (*Client, error) {
	var c Client
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) (err error) {
		ok, err = get(tx, bucketClients, id, &c)
		return err
	})
	if err != nil || !ok {
		return nil, err
	}
	return &c, nil
}

// TouchClient records the use of a registered client, the cleanup removes clients unused for long.
func (s *Store) TouchClient(id string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var c Client
		ok, err := get(tx, bucketClients, id, &c)
		if err != nil || !ok {
			return err
		}
		c.LastUsedAt = now
		return put(tx, bucketClients, id, &c)
	})
}

// CleanupStats counts what Cleanup removed.
type CleanupStats struct {
	Tokens, Grants, Clients int
}

// Cleanup removes expired tokens, grants that ended or have no refresh token left, and registered clients unused
// for clientTTL.
func (s *Store) Cleanup(now time.Time, clientTTL time.Duration) (CleanupStats, error) {
	var st CleanupStats
	err := s.db.Update(func(tx *bolt.Tx) error {
		grants, tokens, clients := tx.Bucket(bucketGrants), tx.Bucket(bucketTokens), tx.Bucket(bucketClients)

		liveGrants := map[string]bool{}
		var deadTokens [][]byte
		err := tokens.ForEach(func(k, v []byte) error {
			var r tokenRecord
			if json.Unmarshal(v, &r) != nil || now.After(r.ExpiresAt) || grants.Get([]byte(r.GrantID)) == nil {
				deadTokens = append(deadTokens, k)
				return nil
			}
			if r.Kind == kindRefresh && r.RotatedAt.IsZero() {
				liveGrants[r.GrantID] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range deadTokens {
			if err := tokens.Delete(k); err != nil {
				return err
			}
		}
		st.Tokens = len(deadTokens)

		var deadGrants [][]byte
		err = grants.ForEach(func(k, v []byte) error {
			var g Grant
			if json.Unmarshal(v, &g) != nil || !liveGrants[g.ID] || (!g.ExpiresAt.IsZero() && now.After(g.ExpiresAt)) {
				deadGrants = append(deadGrants, k)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range deadGrants {
			if err := grants.Delete(k); err != nil {
				return err
			}
		}
		st.Grants = len(deadGrants)

		var deadClients [][]byte
		err = clients.ForEach(func(k, v []byte) error {
			var c Client
			if json.Unmarshal(v, &c) != nil {
				deadClients = append(deadClients, k)
				return nil
			}
			last := c.LastUsedAt
			if last.IsZero() {
				last = c.CreatedAt
			}
			if now.Sub(last) > clientTTL {
				deadClients = append(deadClients, k)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range deadClients {
			if err := clients.Delete(k); err != nil {
				return err
			}
		}
		st.Clients = len(deadClients)
		return nil
	})
	return st, err
}

// Counts returns the number of grants (signed-in user sessions) and registered clients, for metrics.
func (s *Store) Counts() (grants, clients int, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		grants = tx.Bucket(bucketGrants).Stats().KeyN
		clients = tx.Bucket(bucketClients).Stats().KeyN
		return nil
	})
	return grants, clients, err
}
