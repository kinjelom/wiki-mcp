// Package config loads the wiki-mcp configuration file.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Engines lists the supported wiki engines.
var Engines = []string{"xwiki", "dokuwiki"}

type Config struct {
	Server Server `yaml:"server"`
	Wiki   Wiki   `yaml:"wiki"`
	OAuth  OAuth  `yaml:"oauth"`
}

type Server struct {
	// PublicURL is the base URL of the server as clients reach it (env WIKI_MCP_PUBLIC_URL); the MCP endpoint is
	// PublicURL + "/mcp" and it is also the OAuth issuer.
	PublicURL string `yaml:"public_url"`
}

type Wiki struct {
	// Engine is "xwiki" or "dokuwiki".
	Engine string `yaml:"engine"`
	// Name is shown on the sign-in page and in the tool descriptions.
	Name string `yaml:"name"`
	// URL is the base URL the server calls (env WIKI_MCP_WIKI_URL).
	URL string `yaml:"url"`
	// PublicURL is the base URL of the page links given to users, URL when empty.
	PublicURL          string        `yaml:"public_url"`
	Timeout            time.Duration `yaml:"timeout"`
	CAFile             string        `yaml:"ca_file"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
	// SecretLabel is the label of the password field on the sign-in page.
	SecretLabel string `yaml:"secret_label"`
	XWiki       XWiki  `yaml:"xwiki"`
}

type XWiki struct {
	// Wiki is the main (sub)wiki: page IDs without a "wiki:" prefix are in this wiki.
	Wiki string `yaml:"wiki"`
}

type OAuth struct {
	// DataDir holds the store (wiki-mcp.db) and the generated encryption key.
	DataDir string `yaml:"data_dir"`
	// EncryptionKeyFile holds the key (64 hex digits or base64 of 32 bytes) encrypting the stored wiki
	// credentials; created with a random key when missing. Env WIKI_MCP_ENCRYPTION_KEY gives the key itself.
	EncryptionKeyFile   string        `yaml:"encryption_key_file"`
	AccessTokenTTL      time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL     time.Duration `yaml:"refresh_token_ttl"`
	SessionMaxAge       time.Duration `yaml:"session_max_age"`
	RefreshReuseGrace   time.Duration `yaml:"refresh_reuse_grace"`
	DynamicRegistration bool          `yaml:"dynamic_registration"`
	RegisteredClientTTL time.Duration `yaml:"registered_client_ttl"`
	// ClientMetadataHosts are the hosts client ID metadata documents may be fetched from (nil: claude.ai).
	ClientMetadataHosts []string `yaml:"client_metadata_hosts"`
	// AllowedRedirectURIs restricts the OAuth redirects (nil: Claude and Claude Code).
	AllowedRedirectURIs []string `yaml:"allowed_redirect_uris"`
	TrustForwardedFor   bool     `yaml:"trust_forwarded_for"`
}

func Default() Config {
	return Config{
		Wiki: Wiki{
			Timeout: 30 * time.Second,
			XWiki:   XWiki{Wiki: "xwiki"},
		},
		OAuth: OAuth{
			DataDir:             "data",
			AccessTokenTTL:      time.Hour,
			RefreshTokenTTL:     30 * 24 * time.Hour,
			SessionMaxAge:       90 * 24 * time.Hour,
			RefreshReuseGrace:   30 * time.Second,
			DynamicRegistration: true,
			RegisteredClientTTL: 90 * 24 * time.Hour,
		},
	}
}

// Load reads the configuration file (optional, path "" uses the defaults and the environment), applies the
// environment overrides and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return cfg, err
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	if v := os.Getenv("WIKI_MCP_PUBLIC_URL"); v != "" {
		cfg.Server.PublicURL = v
	}
	if v := os.Getenv("WIKI_MCP_WIKI_URL"); v != "" {
		cfg.Wiki.URL = v
	}
	if v := os.Getenv("WIKI_MCP_WIKI_ENGINE"); v != "" {
		cfg.Wiki.Engine = v
	}
	cfg.Server.PublicURL = strings.TrimRight(cfg.Server.PublicURL, "/")
	cfg.Wiki.URL = strings.TrimRight(cfg.Wiki.URL, "/")
	cfg.Wiki.PublicURL = strings.TrimRight(cfg.Wiki.PublicURL, "/")
	if cfg.Wiki.PublicURL == "" {
		cfg.Wiki.PublicURL = cfg.Wiki.URL
	}
	if cfg.OAuth.EncryptionKeyFile == "" {
		cfg.OAuth.EncryptionKeyFile = filepath.Join(cfg.OAuth.DataDir, "encryption.key")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var errs []error
	if err := checkURL("server.public_url", c.Server.PublicURL); err != nil {
		errs = append(errs, err)
	} else if u, _ := url.Parse(c.Server.PublicURL); u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("server.public_url must not have a query or fragment"))
	}
	known := false
	for _, e := range Engines {
		known = known || c.Wiki.Engine == e
	}
	if !known {
		errs = append(errs, fmt.Errorf("wiki.engine must be one of %s, got %q", strings.Join(Engines, ", "), c.Wiki.Engine))
	}
	if err := checkURL("wiki.url", c.Wiki.URL); err != nil {
		errs = append(errs, err)
	}
	if err := checkURL("wiki.public_url", c.Wiki.PublicURL); err != nil {
		errs = append(errs, err)
	}
	if c.Wiki.Timeout <= 0 {
		errs = append(errs, errors.New("wiki.timeout must be positive"))
	}
	o := c.OAuth
	if o.DataDir == "" {
		errs = append(errs, errors.New("oauth.data_dir is required"))
	}
	if o.AccessTokenTTL < time.Minute || o.RefreshTokenTTL < o.AccessTokenTTL {
		errs = append(errs, errors.New("oauth.access_token_ttl must be at least 1m and refresh_token_ttl not shorter"))
	}
	if o.SessionMaxAge < 0 || o.RefreshReuseGrace < 0 || o.RegisteredClientTTL <= 0 {
		errs = append(errs, errors.New("oauth.session_max_age and refresh_reuse_grace must not be negative, registered_client_ttl must be positive"))
	}
	return errors.Join(errs...)
}

func checkURL(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL, got %q", name, v)
	}
	return nil
}

// HTTPClient is the client the server calls the wiki with.
func (w Wiki) HTTPClient() (*http.Client, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: w.InsecureSkipVerify} //nolint:gosec // opt-in for test wikis
	if w.CAFile != "" {
		pem, err := os.ReadFile(w.CAFile)
		if err != nil {
			return nil, fmt.Errorf("wiki.ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("wiki.ca_file %s: no PEM certificates", w.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.MaxIdleConnsPerHost = 16
	return &http.Client{Transport: transport, Timeout: w.Timeout}, nil
}
