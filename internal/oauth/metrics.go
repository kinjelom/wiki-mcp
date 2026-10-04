package oauth

import (
	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	logins        *prometheus.CounterVec
	tokensIssued  *prometheus.CounterVec
	tokenErrors   *prometheus.CounterVec
	refreshReuse  *prometheus.CounterVec
	registrations prometheus.Counter
}

func newMetrics(reg prometheus.Registerer, store *Store) *metrics {
	m := &metrics{
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wiki_mcp_oauth_logins_total",
			Help: "Sign-ins on the authorization page by result (success, invalid_credentials, rate_limited, error).",
		}, []string{"result"}),
		tokensIssued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wiki_mcp_oauth_tokens_issued_total",
			Help: "Token responses by grant type.",
		}, []string{"grant_type"}),
		tokenErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wiki_mcp_oauth_token_errors_total",
			Help: "Failed token requests by grant type and OAuth error.",
		}, []string{"grant_type", "error"}),
		refreshReuse: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wiki_mcp_oauth_refresh_token_reuse_total",
			Help: "Refresh tokens used again, tolerated within the grace period or revoking the session.",
		}, []string{"action"}),
		registrations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wiki_mcp_oauth_client_registrations_total",
			Help: "Dynamic client registrations.",
		}),
	}
	if reg == nil {
		return m
	}
	reg.MustRegister(m.logins, m.tokensIssued, m.tokenErrors, m.refreshReuse, m.registrations,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wiki_mcp_oauth_sessions",
			Help: "Sessions (signed-in user and client pairs) in the store.",
		}, func() float64 {
			g, _, _ := store.Counts()
			return float64(g)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wiki_mcp_oauth_registered_clients",
			Help: "Dynamically registered clients in the store.",
		}, func() float64 {
			_, c, _ := store.Counts()
			return float64(c)
		}),
	)
	return m
}
