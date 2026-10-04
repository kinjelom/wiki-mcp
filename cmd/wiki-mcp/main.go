// Command wiki-mcp is a remote MCP server for XWiki and DokuWiki: users connect it in Claude (or another MCP client)
// and sign in with their own wiki account through OAuth, the tools then search and read the wiki with that account's
// permissions.
package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/kinjelom/wiki-mcp/internal/config"
	"github.com/kinjelom/wiki-mcp/internal/mcpserver"
	"github.com/kinjelom/wiki-mcp/internal/oauth"
	"github.com/kinjelom/wiki-mcp/internal/wiki"
	"github.com/kinjelom/wiki-mcp/internal/wiki/dokuwiki"
	"github.com/kinjelom/wiki-mcp/internal/wiki/xwiki"
)

const defaultListenAddress = ":8090"

func main() {
	var (
		configFile = kingpin.Flag("config.file", "Path to the configuration file.").
				Envar("WIKI_MCP_CONFIG_FILE").String()
		configCheck    = kingpin.Flag("config.check", "Validate the configuration and exit.").Bool()
		metricsAddress = kingpin.Flag("web.metrics-address",
			"Address of the separate listener serving /metrics, empty to disable.").Default(":9407").String()
		toolkitFlags = kingpinflag.AddFlags(kingpin.CommandLine, defaultListenAddress)
		logConfig    = &promslog.Config{}
	)
	promslogflag.AddFlags(kingpin.CommandLine, logConfig)
	kingpin.Version(version.Print("wiki-mcp"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promslog.New(logConfig)

	if err := run(logger, *configFile, *configCheck, *metricsAddress, toolkitFlags); err != nil {
		logger.Error("wiki-mcp failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, configFile string, configCheck bool, metricsAddress string, toolkitFlags *web.FlagConfig) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	engine, err := newEngine(cfg.Wiki)
	if err != nil {
		return err
	}
	if configCheck {
		logger.Info("configuration is valid")
		return nil
	}
	if !strings.HasPrefix(cfg.Server.PublicURL, "https://") {
		logger.Warn("server.public_url is not HTTPS: Claude and most MCP clients require HTTPS", "public_url", cfg.Server.PublicURL)
	}

	key, err := encryptionKey(cfg.OAuth, logger)
	if err != nil {
		return err
	}
	store, err := oauth.OpenStore(filepath.Join(cfg.OAuth.DataDir, "wiki-mcp.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		versioncollector.NewCollector("wiki_mcp"),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	info := engine.Info()
	resource := cfg.Server.PublicURL + "/mcp"
	authServer, err := oauth.New(oauth.Config{
		Issuer:              cfg.Server.PublicURL,
		Resource:            resource,
		WikiName:            info.Name,
		WikiURL:             info.URL,
		SecretLabel:         cfg.Wiki.SecretLabel,
		AccessTokenTTL:      cfg.OAuth.AccessTokenTTL,
		RefreshTokenTTL:     cfg.OAuth.RefreshTokenTTL,
		SessionMaxAge:       cfg.OAuth.SessionMaxAge,
		RefreshReuseGrace:   cfg.OAuth.RefreshReuseGrace,
		ClientMetadataHosts: cfg.OAuth.ClientMetadataHosts,
		AllowedRedirectURIs: cfg.OAuth.AllowedRedirectURIs,
		DynamicRegistration: cfg.OAuth.DynamicRegistration,
		ClientTTL:           cfg.OAuth.RegisteredClientTTL,
		TrustForwardedFor:   cfg.OAuth.TrustForwardedFor,
	}, store, key, engine, logger, reg)
	if err != nil {
		return err
	}

	mcpServer := mcpserver.New(mcpserver.Options{
		Engine:     engine,
		Version:    version.Version,
		Revoke:     authServer.RevokeGrant,
		Logger:     logger,
		Registerer: reg,
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{
		// the tools need no server-to-client requests: no sessions to lose on restarts or behind load balancers
		Stateless:    true,
		JSONResponse: true,
		// every request carries a bearer token, DNS rebinding cannot reach anything; the check would reject a
		// reverse proxy on the same host
		DisableLocalhostProtection: true,
		Logger:                     logger,
	})

	mux := http.NewServeMux()
	if err := authServer.Register(mux); err != nil {
		return err
	}
	mcpPath := mustPath(resource)
	mux.Handle(mcpPath, mcpCORS(authServer.Protect(mcpHandler)))
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Healthy\n"))
	})
	mux.Handle("/{$}", landingPage(info, resource, version.Version))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go authServer.Run(ctx)

	if metricsAddress != "" {
		go serveMetrics(ctx, metricsAddress, reg, logger)
	}

	logger.Info("starting wiki-mcp", "version", version.Info(), "build_context", version.BuildContext(),
		"engine", info.Engine, "wiki", cfg.Wiki.URL, "mcp", resource)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := web.ListenAndServe(server, toolkitFlags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("wiki-mcp stopped")
	return nil
}

func newEngine(w config.Wiki) (wiki.Engine, error) {
	client, err := w.HTTPClient()
	if err != nil {
		return nil, err
	}
	userAgent := "wiki-mcp/" + version.Version
	switch w.Engine {
	case "xwiki":
		return xwiki.New(xwiki.Config{
			Name: w.Name, URL: w.URL, PublicURL: w.PublicURL, Wiki: w.XWiki.Wiki, HTTPClient: client, UserAgent: userAgent,
		})
	case "dokuwiki":
		return dokuwiki.New(dokuwiki.Config{
			Name: w.Name, URL: w.URL, PublicURL: w.PublicURL, HTTPClient: client, UserAgent: userAgent,
		})
	}
	return nil, fmt.Errorf("unknown wiki engine %q", w.Engine)
}

// encryptionKey takes the key from WIKI_MCP_ENCRYPTION_KEY, or from the key file (created when missing).
func encryptionKey(o config.OAuth, logger *slog.Logger) ([]byte, error) {
	if v := os.Getenv("WIKI_MCP_ENCRYPTION_KEY"); v != "" {
		return oauth.ParseKey(v)
	}
	key, created, err := oauth.LoadOrCreateKey(o.EncryptionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	if created {
		logger.Warn("generated a new encryption key; back it up, without it the stored sessions are lost",
			"file", o.EncryptionKeyFile)
	}
	return key, nil
}

func mustPath(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		panic(err)
	}
	return p.EscapedPath()
}

// mcpCORS lets browser-based MCP clients call the endpoint and read the authentication challenge.
func mcpCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", "*")
		hd.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
		hd.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>wiki-mcp: {{.Name}}</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:640px;margin:40px auto;padding:0 16px}code{background:#8882;padding:2px 5px;border-radius:4px}</style>
</head><body>
<h1>{{.Name}} for Claude</h1>
<p>MCP server for the {{.Engine}} wiki <a href="{{.URL}}">{{.URL}}</a>.</p>
<p>In Claude open <b>Settings → Connectors → Add custom connector</b> and enter the URL<br><code>{{.Resource}}</code><br>
then sign in with your wiki account.</p>
<p><small>wiki-mcp {{.Version}}</small></p>
</body></html>`))

func landingPage(info wiki.Info, resource, ver string) http.Handler {
	data := map[string]string{"Name": info.Name, "Engine": info.Engine, "URL": info.URL, "Resource": resource, "Version": ver}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = landingTemplate.Execute(w, data)
	})
}

func serveMetrics(ctx context.Context, address string, reg *prometheus.Registry, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	}))
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	logger.Info("serving metrics", "address", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics listener failed", "err", err)
	}
}
