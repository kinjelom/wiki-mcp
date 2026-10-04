package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cimdMaxBytes = 64 << 10
	cimdCacheTTL = time.Hour
	cimdTimeout  = 5 * time.Second
)

// clientInfo is what the authorization endpoint needs to know about a client.
type clientInfo struct {
	ID           string
	Name         string
	RedirectURIs []string
	// Registered is set for clients from dynamic registration, unset for metadata documents.
	Registered *Client
}

// resolveClient finds the client: a client ID metadata document when the client ID is an HTTPS URL (MCP
// 2025-11-25), otherwise a client from dynamic registration.
func (s *Server) resolveClient(ctx context.Context, clientID string) (*clientInfo, error) {
	if strings.HasPrefix(clientID, "https://") {
		return s.metadataDocument(ctx, clientID)
	}
	c, err := s.store.Client(clientID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("unknown client %q", clientID)
	}
	return &clientInfo{ID: c.ClientID, Name: c.ClientName, RedirectURIs: c.RedirectURIs, Registered: c}, nil
}

type clientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type cachedMetadata struct {
	info    *clientInfo
	fetched time.Time
}

// metadataCache caches fetched client ID metadata documents.
type metadataCache struct {
	mu      sync.Mutex
	entries map[string]cachedMetadata
}

// metadataDocument fetches and validates a client ID metadata document. Only documents on the hosts in
// oauth.client_metadata_hosts are fetched: the URL comes from an unauthenticated request, fetching any URL would
// let anyone make the server send requests into the internal network.
func (s *Server) metadataDocument(ctx context.Context, clientID string) (*clientInfo, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("client ID %q is not a valid metadata document URL", clientID)
	}
	if !hostAllowed(u.Hostname(), s.cfg.ClientMetadataHosts) {
		return nil, fmt.Errorf("client metadata documents from %s are not allowed (oauth.client_metadata_hosts)", u.Hostname())
	}

	s.metadata.mu.Lock()
	if e, ok := s.metadata.entries[clientID]; ok && s.now().Sub(e.fetched) < cimdCacheTTL {
		s.metadata.mu.Unlock()
		return e.info, nil
	}
	s.metadata.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.metadataClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the client metadata document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the client metadata document: HTTP %d", resp.StatusCode)
	}
	var md clientMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, cimdMaxBytes)).Decode(&md); err != nil {
		return nil, fmt.Errorf("decoding the client metadata document: %w", err)
	}
	if md.ClientID != clientID {
		return nil, fmt.Errorf("the client metadata document has client_id %q, not its own URL", md.ClientID)
	}
	if len(md.RedirectURIs) == 0 {
		return nil, errors.New("the client metadata document has no redirect_uris")
	}
	if m := md.TokenEndpointAuthMethod; m != "" && m != "none" {
		return nil, fmt.Errorf("token_endpoint_auth_method %q of the client metadata document is not supported", m)
	}
	info := &clientInfo{ID: clientID, Name: md.ClientName, RedirectURIs: md.RedirectURIs}
	if info.Name == "" {
		info.Name = u.Hostname()
	}
	s.metadata.mu.Lock()
	s.metadata.entries[clientID] = cachedMetadata{info: info, fetched: s.now()}
	s.metadata.mu.Unlock()
	return info, nil
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, a := range allowed {
		a = strings.ToLower(a)
		if a == host || a == "*" {
			return true
		}
		if suffix, ok := strings.CutPrefix(a, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// redirectMatches compares a requested redirect URI with a registered one: exactly, or for loopback redirects of
// native clients (Claude Code) with any port (RFC 8252 section 7.3).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	r, err1 := url.Parse(registered)
	q, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil {
		return false
	}
	return r.Scheme == "http" && q.Scheme == "http" && isLoopback(r.Hostname()) &&
		r.Hostname() == q.Hostname() && r.EscapedPath() == q.EscapedPath() && r.RawQuery == q.RawQuery
}

// redirectAllowed checks a redirect URI against oauth.allowed_redirect_uris: entries match like registered URIs,
// an entry ending with "*" matches by prefix, a sole "*" allows any.
func redirectAllowed(uri string, allowed []string) bool {
	for _, a := range allowed {
		if prefix, ok := strings.CutSuffix(a, "*"); ok && strings.HasPrefix(uri, prefix) {
			return true
		}
		if redirectMatches(a, uri) {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validRedirectURI accepts absolute HTTPS URIs and HTTP loopback URIs without a fragment (OAuth 2.1).
func validRedirectURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("redirect URI %q is not an absolute URL", uri)
	}
	if u.Fragment != "" {
		return fmt.Errorf("redirect URI %q has a fragment", uri)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return fmt.Errorf("redirect URI %q must use https (http only for localhost)", uri)
	}
	return nil
}

// registrationRequest is the client metadata of a dynamic registration (RFC 7591).
type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
}

// handleRegister is the dynamic client registration endpoint (RFC 7591).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON object")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validRedirectURI(u); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
		if !redirectAllowed(u, s.cfg.AllowedRedirectURIs) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				fmt.Sprintf("redirect URI %q is not allowed by this server", u))
			return
		}
	}
	for _, gt := range req.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", fmt.Sprintf("grant type %q is not supported", gt))
			return
		}
	}
	method := req.TokenEndpointAuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_basic", "client_secret_post":
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata",
			fmt.Sprintf("token_endpoint_auth_method %q is not supported", method))
		return
	}

	now := s.now()
	c := &Client{
		ClientID:     randomToken("wmc_"),
		ClientName:   truncate(req.ClientName, 100),
		RedirectURIs: req.RedirectURIs,
		AuthMethod:   method,
		CreatedAt:    now,
	}
	resp := map[string]any{
		"client_id":                  c.ClientID,
		"client_id_issued_at":        now.Unix(),
		"client_name":                c.ClientName,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if method != "none" {
		secret := randomToken("wms_")
		c.SecretHash = hashToken(secret)
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	if err := s.store.PutClient(c); err != nil {
		s.logger.Error("storing a registered client", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	s.metrics.registrations.Inc()
	s.logger.Info("client registered", "client_id", c.ClientID, "client_name", c.ClientName, "redirect_uris", c.RedirectURIs)
	writeJSON(w, http.StatusCreated, resp)
}

// authenticateClient checks the client secret of a confidential registered client at the token endpoint; public
// clients (metadata documents, registrations with "none") need only their client ID.
func (s *Server) authenticateClient(r *http.Request) (clientID string, err error) {
	id, secret, basic := r.BasicAuth()
	if basic {
		if id, err = url.QueryUnescape(id); err != nil {
			return "", errors.New("invalid client credentials")
		}
		if secret, err = url.QueryUnescape(secret); err != nil {
			return "", errors.New("invalid client credentials")
		}
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" {
		return "", errors.New("client_id is required")
	}
	if strings.HasPrefix(id, "https://") {
		return id, nil
	}
	c, err := s.store.Client(id)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", errors.New("unknown client")
	}
	if c.SecretHash != "" && !equalHash(c.SecretHash, hashToken(secret)) {
		return "", errors.New("invalid client credentials")
	}
	return id, nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
