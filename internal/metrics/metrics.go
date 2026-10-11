// Package metrics holds notifco's Prometheus collectors.
//
// The brief asks for four: polls/min, alert latency, active users, active
// subscriptions. The rest exist because debugging the first four without them
// is guesswork.
//
// Honest caveat on alert latency. The upstream API exposes no "last updated"
// field on a class, so the instant a seat actually opened is unknowable.
// AlertLatency measures detected -> message sent, which is the part notifco
// controls. Detection lag is bounded by the poll interval and is reported
// separately as a configured bound, not a measurement.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the collector set.
type Metrics struct {
	reg *prometheus.Registry

	PollsTotal    *prometheus.CounterVec
	PollCycle     prometheus.Histogram
	APIRequests   *prometheus.CounterVec
	APILatency    *prometheus.HistogramVec
	RateLimitWait prometheus.Histogram

	TransitionsTotal prometheus.Counter

	NotificationsTotal *prometheus.CounterVec
	AlertLatency       prometheus.Histogram
	QueueDepth         prometheus.Gauge

	ActiveUsers         prometheus.Gauge
	ActiveSubscriptions prometheus.Gauge

	PollIntervalBound prometheus.Gauge
}

// New builds the collector set with its own registry, so tests can create
// independent instances without duplicate-registration panics.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,

		PollsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifco_polls_total",
			Help: "Course polls attempted, by tier and result. polls/min is rate() of this.",
		}, []string{"tier", "result"}),

		PollCycle: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "notifco_poll_cycle_seconds",
			Help:    "Duration of one poll of a single course.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}),

		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifco_api_requests_total",
			Help: "Upstream API requests by endpoint and HTTP status (0 = transport error).",
		}, []string{"endpoint", "code"}),

		APILatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notifco_api_request_seconds",
			Help:    "Upstream API request duration by endpoint.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint"}),

		RateLimitWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "notifco_rate_limit_wait_seconds",
			Help:    "Time blocked on the upstream token bucket.",
			Buckets: []float64{.001, .01, .05, .1, .5, 1, 5, 10},
		}),

		TransitionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "notifco_transitions_total",
			Help: "Full-to-open transitions detected.",
		}),

		NotificationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifco_notifications_total",
			Help: "Notification outcomes by status (sent/retry/dead_letter) and transport.",
		}, []string{"status", "via"}),

		AlertLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "notifco_alert_latency_seconds",
			Help: "Seat opening DETECTED to message sent. Not seat-open to sent: " +
				"the upstream exposes no timestamp, so the true open instant is unknowable.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300},
		}),

		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifco_queue_depth",
			Help: "Notifications pending delivery.",
		}),

		ActiveUsers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifco_active_users",
			Help: "Users with at least one active subscription.",
		}),

		ActiveSubscriptions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifco_active_subscriptions",
			Help: "Active subscription rows.",
		}),

		PollIntervalBound: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifco_poll_interval_seconds",
			Help: "Configured base poll interval. This bounds detection lag.",
		}),
	}

	reg.MustRegister(
		m.PollsTotal, m.PollCycle, m.APIRequests, m.APILatency, m.RateLimitWait,
		m.TransitionsTotal, m.NotificationsTotal, m.AlertLatency, m.QueueDepth,
		m.ActiveUsers, m.ActiveSubscriptions, m.PollIntervalBound,
	)
	return m
}

// Registry exposes the registry for serving.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler returns an http.Handler serving /metrics and /healthz.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// ObserveRequest implements uwapi.Observer.
func (m *Metrics) ObserveRequest(endpoint string, status int, dur time.Duration) {
	// Endpoint labels must stay low-cardinality: use the first path segment
	// (e.g. "ClassSchedules") rather than the full path with term and course.
	ep := firstSegment(endpoint)
	m.APIRequests.WithLabelValues(ep, statusLabel(status)).Inc()
	m.APILatency.WithLabelValues(ep).Observe(dur.Seconds())
}

// ObserveRateLimitWait implements uwapi.Observer.
func (m *Metrics) ObserveRateLimitWait(d time.Duration) {
	m.RateLimitWait.Observe(d.Seconds())
}

func firstSegment(path string) string {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return path
}

func statusLabel(status int) string {
	switch {
	case status == 0:
		return "error"
	case status >= 200 && status < 300:
		return "2xx"
	case status == 404:
		return "404"
	case status == 429:
		return "429"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
