// Package oauth is the OAuth 2.1 authorization server of the MCP endpoint. Users sign in with their wiki account:
// the sign-in page checks the credentials against the wiki, and the server keeps them, encrypted, to call the wiki
// as that user. Clients identify themselves with a client ID metadata document (Claude's
// https://claude.ai/oauth/mcp-oauth-client-metadata) or register dynamically (RFC 7591); every authorization uses
// PKCE S256, refresh tokens are rotated and a replayed one revokes the session.
package oauth

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

const (
	scopeWiki    = "wiki"
	scopeOffline = "offline_access"

	codeTTL          = time.Minute
	authorizeFormTTL = 15 * time.Minute
	loginTimeout     = 15 * time.Second
	cleanupInterval  = 10 * time.Minute
)

var supportedScopes = []string{scopeWiki, scopeOffline}

// DefaultRedirectURIs are the redirect URIs allowed when oauth.allowed_redirect_uris is not set: the Claude apps
// (claude.ai, Desktop, mobile) and the loopback redirects of Claude Code.
var DefaultRedirectURIs = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
	"http://localhost/callback",
	"http://127.0.0.1/callback",
}

// DefaultClientMetadataHosts are the hosts client ID metadata documents are fetched from by default.
var DefaultClientMetadataHosts = []string{"claude.ai"}

type Config struct {
	// Issuer is the public base URL of the server, the OAuth issuer, e.g. https://wiki-mcp.example.com.
	Issuer string
	// Resource is the URL of the MCP endpoint (Issuer + "/mcp"): the protected resource.
	Resource string
	// WikiName and WikiURL are shown on the sign-in page.
	WikiName string
	WikiURL  string
	// SecretLabel is the label of the password field.
	SecretLabel string

	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// SessionMaxAge ends a session after this time regardless of refreshes, the user signs in again (0: never).
	SessionMaxAge time.Duration
	// RefreshReuseGrace tolerates a second use of a refresh token for this long (a client retrying a refresh
	// whose response it lost); later reuse revokes the session.
	RefreshReuseGrace time.Duration

	ClientMetadataHosts []string
	AllowedRedirectURIs []string
	DynamicRegistration bool
	// ClientTTL removes dynamically registered clients unused for this long.
	ClientTTL time.Duration
	// TrustForwardedFor takes the client address for the sign-in rate limit from X-Forwarded-For.
	TrustForwardedFor bool
}

// Authenticator checks credentials against the wiki.
type Authenticator interface {
	Login(ctx context.Context, cred wiki.Credential) (*wiki.Identity, error)
}

type Server struct {
	cfg            Config
	store          *Store
	sealer         *sealer
	auth           Authenticator
	logger         *slog.Logger
	now            func() time.Time
	metadata       metadataCache
	metadataClient *http.Client
	codes          codeStore
	limiter        *limiter
	metrics        *metrics
	login          *template.Template
	issuerPath     string
}

//go:embed login.html
var loginHTML string

func New(cfg Config, store *Store, key []byte, authenticator Authenticator, logger *slog.Logger, reg prometheus.Registerer) (*Server, error) {
	iss, err := url.Parse(cfg.Issuer)
	if err != nil || iss.Scheme == "" || iss.Host == "" {
		return nil, fmt.Errorf("invalid issuer URL %q", cfg.Issuer)
	}
	sealer, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	if cfg.AllowedRedirectURIs == nil {
		cfg.AllowedRedirectURIs = DefaultRedirectURIs
	}
	if cfg.ClientMetadataHosts == nil {
		cfg.ClientMetadataHosts = DefaultClientMetadataHosts
	}
	if cfg.SecretLabel == "" {
		cfg.SecretLabel = "Password"
	}
	s := &Server{
		cfg:            cfg,
		store:          store,
		sealer:         sealer,
		auth:           authenticator,
		logger:         logger,
		now:            time.Now,
		metadata:       metadataCache{entries: map[string]cachedMetadata{}},
		metadataClient: &http.Client{Timeout: cimdTimeout},
		codes:          codeStore{codes: map[string]*pendingCode{}},
		limiter:        newLimiter(10, 15*time.Minute),
		login:          template.Must(template.New("login").Parse(loginHTML)),
		issuerPath:     strings.TrimRight(iss.EscapedPath(), "/"),
	}
	s.metrics = newMetrics(reg, store)
	return s, nil
}

// Register adds the OAuth endpoints and the discovery documents to mux.
func (s *Server) Register(mux *http.ServeMux) error {
	res, err := url.Parse(s.cfg.Resource)
	if err != nil {
		return err
	}
	resourcePath := strings.TrimRight(res.EscapedPath(), "/")

	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.cfg.Resource,
		AuthorizationServers:   []string{s.cfg.Issuer},
		ScopesSupported:        []string{scopeWiki},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           s.cfg.WikiName,
	})
	asm := cors(http.HandlerFunc(s.handleMetadata))

	// RFC 9728 / RFC 8414 insert the well-known segment before the path; some clients append it instead
	paths := map[string]http.Handler{
		"/.well-known/oauth-protected-resource" + resourcePath:   prm,
		"/.well-known/oauth-protected-resource":                  prm,
		"/.well-known/oauth-authorization-server" + s.issuerPath: asm,
	}
	if s.issuerPath != "" {
		paths[s.issuerPath+"/.well-known/oauth-protected-resource"] = prm
		paths[s.issuerPath+"/.well-known/oauth-authorization-server"] = asm
	}
	for p, h := range paths {
		mux.Handle(p, h)
	}
	mux.HandleFunc(s.issuerPath+"/authorize", s.handleAuthorize)
	mux.Handle(s.issuerPath+"/token", cors(http.HandlerFunc(s.handleToken)))
	mux.Handle(s.issuerPath+"/revoke", cors(http.HandlerFunc(s.handleRevoke)))
	if s.cfg.DynamicRegistration {
		mux.Handle(s.issuerPath+"/register", cors(http.HandlerFunc(s.post(s.handleRegister))))
	}
	return nil
}

// ResourceMetadataURL is the URL of the protected resource metadata, given in the WWW-Authenticate header.
func (s *Server) ResourceMetadataURL() string {
	res, _ := url.Parse(s.cfg.Resource)
	return res.Scheme + "://" + res.Host + "/.well-known/oauth-protected-resource" + strings.TrimRight(res.EscapedPath(), "/")
}

// Protect requires a valid access token for h and puts the user into the request context (UserFromTokenInfo).
func (s *Server) Protect(h http.Handler) http.Handler {
	return auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.ResourceMetadataURL(),
		ClockSkew:           30 * time.Second,
	})(h)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m := map[string]any{
		"issuer":                                s.cfg.Issuer,
		"authorization_endpoint":                s.cfg.Issuer + "/authorize",
		"token_endpoint":                        s.cfg.Issuer + "/token",
		"revocation_endpoint":                   s.cfg.Issuer + "/revoke",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"scopes_supported":                      supportedScopes,
		// MCP 2025-11-25: Claude uses its metadata document only when both this and "none" above are advertised
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
	if s.cfg.DynamicRegistration {
		m["registration_endpoint"] = s.cfg.Issuer + "/register"
	}
	writeJSON(w, http.StatusOK, m)
}

// authzRequest is a validated authorization request; it travels sealed in the sign-in form, so the server keeps
// no state for pages that are never submitted.
type authzRequest struct {
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	RedirectURI   string    `json:"redirect_uri"`
	State         string    `json:"state,omitempty"`
	CodeChallenge string    `json:"code_challenge"`
	Scopes        []string  `json:"scopes"`
	ExpiresAt     time.Time `json:"exp"`
}

const purposeAuthorize = "authorize"

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.authorizeStart(w, r)
	case http.MethodPost:
		s.authorizeSubmit(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) authorizeStart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, redirectURI := q.Get("client_id"), q.Get("redirect_uri")
	if clientID == "" {
		s.renderFatal(w, "The request has no client_id.")
		return
	}
	client, err := s.resolveClient(r.Context(), clientID)
	if err != nil {
		s.logger.Warn("authorization request from an unknown client", "client_id", clientID, "err", err)
		s.renderFatal(w, "The application is not known to this server: "+err.Error())
		return
	}
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if !slices.ContainsFunc(client.RedirectURIs, func(reg string) bool { return redirectMatches(reg, redirectURI) }) {
		s.renderFatal(w, "The redirect URI is not registered for this application.")
		return
	}
	if !redirectAllowed(redirectURI, s.cfg.AllowedRedirectURIs) {
		s.logger.Warn("authorization request with a redirect URI that is not allowed", "client_id", clientID, "redirect_uri", redirectURI)
		s.renderFatal(w, "This server does not allow redirects to "+redirectURI+" (oauth.allowed_redirect_uris).")
		return
	}
	if client.Registered != nil {
		if err := s.store.TouchClient(client.ID, s.now()); err != nil {
			s.logger.Warn("updating a registered client", "err", err)
		}
	}

	// the client and redirect URI are known now: further errors go back to the client
	state := q.Get("state")
	fail := func(code, desc string) { s.redirectError(w, r, redirectURI, state, code, desc) }
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the authorization code flow is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || len(challenge) < 43 || len(challenge) > 128 {
		fail("invalid_request", "PKCE with code_challenge_method S256 is required")
		return
	}
	if res := q.Get("resource"); res != "" && !sameResource(res, s.cfg.Resource) {
		fail("invalid_target", "unknown resource "+res)
		return
	}
	req := authzRequest{
		ClientID:      client.ID,
		ClientName:    client.Name,
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: challenge,
		Scopes:        parseScopes(q.Get("scope")),
		ExpiresAt:     s.now().Add(authorizeFormTTL),
	}
	s.renderLogin(w, http.StatusOK, req, "", "")
}

func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.renderFatal(w, "The form could not be read.")
		return
	}
	req, err := s.openAuthzRequest(r.PostForm.Get("request"))
	if err != nil {
		s.renderFatal(w, "This sign-in page has expired.")
		return
	}
	if r.PostForm.Get("action") == "deny" {
		s.redirectError(w, r, req.RedirectURI, req.State, "access_denied", "the user cancelled the sign-in")
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	if username == "" || password == "" {
		s.renderLogin(w, http.StatusOK, *req, username, "Enter your user name and "+strings.ToLower(s.cfg.SecretLabel)+".")
		return
	}

	now := s.now()
	ipKey, userKey := "ip:"+s.clientIP(r), "user:"+strings.ToLower(username)
	if s.limiter.blocked(ipKey, now) || s.limiter.blocked(userKey, now) {
		s.metrics.logins.WithLabelValues("rate_limited").Inc()
		s.logger.Warn("sign-in rate limited", "user", username, "ip", s.clientIP(r))
		s.renderLogin(w, http.StatusTooManyRequests, *req, username, "Too many failed sign-in attempts, try again in a few minutes.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	cred := wiki.Credential{Username: username, Secret: password}
	identity, err := s.auth.Login(ctx, cred)
	switch {
	case errors.Is(err, wiki.ErrInvalidCredentials):
		s.limiter.failure(ipKey, now)
		s.limiter.failure(userKey, now)
		s.metrics.logins.WithLabelValues("invalid_credentials").Inc()
		s.logger.Info("sign-in rejected by the wiki", "user", username, "ip", s.clientIP(r), "err", err)
		s.renderLogin(w, http.StatusOK, *req, username, "Wrong user name or "+strings.ToLower(s.cfg.SecretLabel)+".")
		return
	case err != nil:
		s.metrics.logins.WithLabelValues("error").Inc()
		s.logger.Error("sign-in check failed", "user", username, "err", err)
		s.renderLogin(w, http.StatusBadGateway, *req, username, "The wiki could not check your account right now, try again later.")
		return
	}
	s.limiter.reset(userKey)
	s.metrics.logins.WithLabelValues("success").Inc()
	s.logger.Info("user signed in", "user", identity.ID, "client", req.ClientName, "client_id", req.ClientID)

	code := randomToken("")
	s.codes.put(code, &pendingCode{req: *req, identity: *identity, credential: cred, expires: now.Add(codeTTL)})
	q := url.Values{"code": {code}, "iss": {s.cfg.Issuer}}
	if req.State != "" {
		q.Set("state", req.State)
	}
	http.Redirect(w, r, appendQuery(req.RedirectURI, q), http.StatusFound)
}

func (s *Server) openAuthzRequest(sealed string) (*authzRequest, error) {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	plain, err := s.sealer.open(b, purposeAuthorize)
	if err != nil {
		return nil, err
	}
	var req authzRequest
	if err := json.Unmarshal(plain, &req); err != nil {
		return nil, err
	}
	if s.now().After(req.ExpiresAt) {
		return nil, errors.New("expired")
	}
	return &req, nil
}

type loginPage struct {
	WikiName, WikiURL      string
	ClientName             string
	RedirectHost           string
	Loopback               bool
	Action, Request        string
	Username, Error, Fatal string
	SecretLabel            string
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, req authzRequest, username, errMsg string) {
	b, _ := json.Marshal(req)
	redirect, _ := url.Parse(req.RedirectURI)
	page := loginPage{
		WikiName:     s.cfg.WikiName,
		WikiURL:      s.cfg.WikiURL,
		ClientName:   req.ClientName,
		RedirectHost: redirect.Host,
		Loopback:     isLoopback(redirect.Hostname()),
		Action:       s.issuerPath + "/authorize",
		Request:      base64.RawURLEncoding.EncodeToString(s.sealer.seal(b, purposeAuthorize)),
		Username:     username,
		Error:        errMsg,
		SecretLabel:  s.cfg.SecretLabel,
	}
	s.renderPage(w, status, page)
}

func (s *Server) renderFatal(w http.ResponseWriter, msg string) {
	s.renderPage(w, http.StatusBadRequest, loginPage{WikiName: s.cfg.WikiName, Fatal: msg})
}

func (s *Server) renderPage(w http.ResponseWriter, status int, page loginPage) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := s.login.Execute(w, page); err != nil {
		s.logger.Error("rendering the sign-in page", "err", err)
	}
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	q := url.Values{"error": {code}, "iss": {s.cfg.Issuer}}
	if desc != "" {
		q.Set("error_description", desc)
	}
	if state != "" {
		q.Set("state", state)
	}
	http.Redirect(w, r, appendQuery(redirectURI, q), http.StatusFound)
}

func appendQuery(uri string, q url.Values) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	existing := u.Query()
	for k, v := range q {
		existing[k] = v
	}
	u.RawQuery = existing.Encode()
	return u.String()
}

func sameResource(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// parseScopes keeps the supported scopes of a request; the wiki scope is always granted.
func parseScopes(scope string) []string {
	scopes := []string{scopeWiki}
	for _, sc := range strings.Fields(scope) {
		if sc == scopeOffline && !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	return scopes
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	grantType := r.PostForm.Get("grant_type")
	clientID, err := s.authenticateClient(r)
	if err != nil {
		s.metrics.tokenErrors.WithLabelValues(grantType, "invalid_client").Inc()
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}
	switch grantType {
	case "authorization_code":
		s.exchangeCode(w, r, clientID)
	case "refresh_token":
		s.refresh(w, r, clientID)
	default:
		s.metrics.tokenErrors.WithLabelValues(grantType, "unsupported_grant_type").Inc()
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (s *Server) tokenError(w http.ResponseWriter, grantType, code, desc string) {
	s.metrics.tokenErrors.WithLabelValues(grantType, code).Inc()
	writeOAuthError(w, http.StatusBadRequest, code, desc)
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, clientID string) {
	const gt = "authorization_code"
	pc := s.codes.take(r.PostForm.Get("code"), s.now())
	if pc == nil {
		s.tokenError(w, gt, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if pc.req.ClientID != clientID {
		s.tokenError(w, gt, "invalid_grant", "the code was issued to another client")
		return
	}
	if uri := r.PostForm.Get("redirect_uri"); uri != "" && uri != pc.req.RedirectURI {
		s.tokenError(w, gt, "invalid_grant", "redirect_uri differs from the authorization request")
		return
	}
	if !equalHash(pkceS256(r.PostForm.Get("code_verifier")), pc.req.CodeChallenge) {
		s.tokenError(w, gt, "invalid_grant", "PKCE verification failed")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" && !sameResource(res, s.cfg.Resource) {
		s.tokenError(w, gt, "invalid_target", "unknown resource "+res)
		return
	}
	now := s.now()
	g := &Grant{
		ID:         randomToken("wmg_"),
		ClientID:   pc.req.ClientID,
		ClientName: pc.req.ClientName,
		Identity:   pc.identity,
		Scopes:     pc.req.Scopes,
		CreatedAt:  now,
	}
	if s.cfg.SessionMaxAge > 0 {
		g.ExpiresAt = now.Add(s.cfg.SessionMaxAge)
	}
	b, _ := json.Marshal(pc.credential)
	g.Credential = s.sealer.seal(b, "credential:"+g.ID)
	s.issue(w, g, gt)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, clientID string) {
	const gt = "refresh_token"
	now := s.now()
	before, err := s.store.RotateRefresh(hashToken(r.PostForm.Get("refresh_token")), now)
	if err != nil {
		s.logger.Error("reading a refresh token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if before == nil || before.Kind != kindRefresh || now.After(before.ExpiresAt) {
		s.tokenError(w, gt, "invalid_grant", "invalid or expired refresh token")
		return
	}
	g, err := s.store.Grant(before.GrantID)
	if err != nil {
		s.logger.Error("reading a grant", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if g == nil {
		s.tokenError(w, gt, "invalid_grant", "the session was revoked, sign in again")
		return
	}
	if g.ClientID != clientID {
		s.tokenError(w, gt, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	if !before.RotatedAt.IsZero() {
		if now.Sub(before.RotatedAt) > s.cfg.RefreshReuseGrace {
			// a used refresh token came back: someone holds a stolen copy, end the session of both
			_ = s.store.DeleteGrant(g.ID)
			s.metrics.refreshReuse.WithLabelValues("revoked").Inc()
			s.logger.Warn("refresh token replayed, session revoked", "user", g.Identity.ID, "client", g.ClientName)
			s.tokenError(w, gt, "invalid_grant", "refresh token reuse detected, the session was revoked")
			return
		}
		s.metrics.refreshReuse.WithLabelValues("grace").Inc()
		s.logger.Info("refresh token reused within the grace period", "user", g.Identity.ID, "client", g.ClientName)
	}
	if !g.ExpiresAt.IsZero() && now.After(g.ExpiresAt) {
		_ = s.store.DeleteGrant(g.ID)
		s.tokenError(w, gt, "invalid_grant", "the session expired, sign in again")
		return
	}
	cred, err := s.credential(g)
	if err != nil {
		_ = s.store.DeleteGrant(g.ID)
		s.logger.Warn("stored credential cannot be decrypted (encryption key changed?), session revoked", "user", g.Identity.ID, "err", err)
		s.tokenError(w, gt, "invalid_grant", "the session is no longer valid, sign in again")
		return
	}
	// the password may have changed or the account been disabled since the sign-in
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	if _, err := s.auth.Login(ctx, cred); errors.Is(err, wiki.ErrInvalidCredentials) {
		_ = s.store.DeleteGrant(g.ID)
		s.logger.Info("the wiki no longer accepts the credentials, session revoked", "user", g.Identity.ID, "err", err)
		s.tokenError(w, gt, "invalid_grant", "the wiki no longer accepts your credentials, sign in again")
		return
	} else if err != nil {
		// do not sign users out while the wiki is down
		s.logger.Warn("credential check on refresh failed, refreshing anyway", "user", g.Identity.ID, "err", err)
	}
	g.RefreshedAt = now
	s.issue(w, g, gt)
}

// issue stores the grant with a new access and refresh token and writes the token response.
func (s *Server) issue(w http.ResponseWriter, g *Grant, grantType string) {
	now := s.now()
	accessExp, refreshExp := now.Add(s.cfg.AccessTokenTTL), now.Add(s.cfg.RefreshTokenTTL)
	if !g.ExpiresAt.IsZero() {
		accessExp, refreshExp = minTime(accessExp, g.ExpiresAt), minTime(refreshExp, g.ExpiresAt)
	}
	access, refresh := randomToken("wma_"), randomToken("wmr_")
	err := s.store.IssueTokens(g,
		hashToken(access), tokenRecord{Kind: kindAccess, GrantID: g.ID, ExpiresAt: accessExp},
		hashToken(refresh), tokenRecord{Kind: kindRefresh, GrantID: g.ID, ExpiresAt: refreshExp})
	if err != nil {
		s.logger.Error("storing tokens", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	s.metrics.tokensIssued.WithLabelValues(grantType).Inc()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessExp.Sub(now).Seconds()),
		"refresh_token": refresh,
		"scope":         strings.Join(g.Scopes, " "),
	})
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// handleRevoke revokes the session of an access or refresh token (RFC 7009).
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	rec, err := s.store.Token(hashToken(r.PostForm.Get("token")))
	if err == nil && rec != nil {
		if err := s.RevokeGrant(rec.GrantID); err != nil {
			s.logger.Error("revoking a session", "err", err)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// RevokeGrant ends a session: its access and refresh tokens stop working at once.
func (s *Server) RevokeGrant(id string) error {
	g, err := s.store.Grant(id)
	if err != nil || g == nil {
		return err
	}
	s.logger.Info("session revoked", "user", g.Identity.ID, "client", g.ClientName)
	return s.store.DeleteGrant(id)
}

// User is the signed-in user of an MCP request.
type User struct {
	GrantID    string
	ClientName string
	Identity   wiki.Identity
	Credential wiki.Credential
}

// userKey is the key of the User in auth.TokenInfo.Extra.
const userKey = "wiki-mcp/user"

// UserFromTokenInfo returns the user of a request authorized by Protect, nil for other requests.
func UserFromTokenInfo(ti *auth.TokenInfo) *User {
	if ti == nil {
		return nil
	}
	u, _ := ti.Extra[userKey].(*User)
	return u
}

func (s *Server) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	rec, err := s.store.Token(hashToken(token))
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Kind != kindAccess {
		return nil, fmt.Errorf("%w: unknown access token", auth.ErrInvalidToken)
	}
	if s.now().After(rec.ExpiresAt) {
		return nil, fmt.Errorf("%w: the access token expired", auth.ErrInvalidToken)
	}
	g, err := s.store.Grant(rec.GrantID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: the session was revoked", auth.ErrInvalidToken)
	}
	cred, err := s.credential(g)
	if err != nil {
		return nil, fmt.Errorf("%w: the session is no longer valid", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		Scopes:     g.Scopes,
		Expiration: rec.ExpiresAt,
		UserID:     g.Identity.ID,
		Extra: map[string]any{userKey: &User{
			GrantID: g.ID, ClientName: g.ClientName, Identity: g.Identity, Credential: cred,
		}},
	}, nil
}

func (s *Server) credential(g *Grant) (wiki.Credential, error) {
	var cred wiki.Credential
	plain, err := s.sealer.open(g.Credential, "credential:"+g.ID)
	if err != nil {
		return cred, err
	}
	return cred, json.Unmarshal(plain, &cred)
}

// Run removes expired sessions, tokens, codes and stale registered clients until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		s.cleanup()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) cleanup() {
	now := s.now()
	st, err := s.store.Cleanup(now, s.cfg.ClientTTL)
	if err != nil {
		s.logger.Error("store cleanup", "err", err)
	} else if st.Tokens+st.Grants+st.Clients > 0 {
		s.logger.Debug("store cleanup", "tokens", st.Tokens, "sessions", st.Grants, "clients", st.Clients)
	}
	s.codes.expire(now)
	s.limiter.expire(now)
	s.metadata.mu.Lock()
	for k, e := range s.metadata.entries {
		if now.Sub(e.fetched) > cimdCacheTTL {
			delete(s.metadata.entries, k)
		}
	}
	s.metadata.mu.Unlock()
}

// pendingCode is an authorization code waiting to be exchanged; codes live a minute and are kept in memory.
type pendingCode struct {
	req        authzRequest
	identity   wiki.Identity
	credential wiki.Credential
	expires    time.Time
}

type codeStore struct {
	mu    sync.Mutex
	codes map[string]*pendingCode
}

func (c *codeStore) put(code string, pc *pendingCode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codes[hashToken(code)] = pc
}

// take returns and removes the code: a code can be exchanged once.
func (c *codeStore) take(code string, now time.Time) *pendingCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := hashToken(code)
	pc := c.codes[key]
	delete(c.codes, key)
	if pc == nil || now.After(pc.expires) {
		return nil
	}
	return pc
}

func (c *codeStore) expire(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, pc := range c.codes {
		if now.After(pc.expires) {
			delete(c.codes, k)
		}
	}
}

// limiter blocks a key (client address, user name) after max failed sign-ins within window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) recent(key string, now time.Time) []time.Time {
	ts := slices.DeleteFunc(l.fails[key], func(t time.Time) bool { return now.Sub(t) > l.window })
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= l.max
}

func (l *limiter) failure(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.recent(key, now), now)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func (l *limiter) expire(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.fails {
		l.recent(k, now)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

// cors allows the OAuth endpoints to be called from browser-based clients (they use no cookies).
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", "*")
		hd.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}
