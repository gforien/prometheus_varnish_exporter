package main

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// prometheusMetric

type prometheusMetric struct {
	NameVarnish string
	Name        string
	Value       float64
	Description string
	Group       string

	gaugeVec *prometheus.GaugeVec
	gauge    prometheus.Gauge
	labels   prometheus.Labels
}

func NewPrometheusMetric(m *varnishMetric) *prometheusMetric {
	pm := &prometheusMetric{
		NameVarnish: m.Name,
		Name:        fullPrometheusMetricName(m),
		Value:       m.Value,
		Description: m.Description,
		Group:       prometheusGroup(m),
	}
	pm.labels = prometheusLabels(pm, m)
	return pm
}

func (p *prometheusMetric) Set(value float64) {
	p.Value = value
	p.Gauge().Set(p.Value)
}

func (p *prometheusMetric) Reset() {
	if p.gaugeVec != nil {
		p.gaugeVec.Reset()
	} else {
		p.gauge.Set(0)
	}
}

func (p *prometheusMetric) Gauge() prometheus.Gauge {
	if p.gaugeVec != nil {
		return p.gaugeVec.With(p.labels)
	} else {
		return p.gauge
	}
}

func (p *prometheusMetric) Labels() string {
	if len(p.labels) > 0 {
		return prettyPrintsMap(p.labels)
	}
	return ""
}

func (p *prometheusMetric) LabelNames() []string {
	// cache so these will be calculated once and the order
	// does not change depending on map range
	var names []string
	for name := range p.labels {
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// prometheusExporter

type prometheusExporter struct {
	namespace string
	metrics   []*prometheusMetric

	up                          prometheus.Gauge
	totalScrapes, failedScrapes prometheus.Counter

	mutex sync.RWMutex
}

func NewPrometheusExporter() *prometheusExporter {
	namespace := "varnish"
	return &prometheusExporter{
		namespace: namespace,
		metrics:   make([]*prometheusMetric, 0),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of varnish successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total varnish scrapes.",
		}),
		failedScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_failed_scrapes",
			Help:      "Current total of varnish scrape errors",
		}),
	}
}

// Implements prometheus.Collector
func (pe *prometheusExporter) Describe(ch chan<- *prometheus.Desc) {
	if StartParams.Verbose {
		defer func(start time.Time) {
			logInfo("prometheus.Collector.Describe  %s", time.Now().Sub(start))
		}(time.Now())
	}

	for _, m := range pe.metrics {
		m.Gauge().Describe(ch)
	}
	ch <- pe.up.Desc()
	ch <- pe.totalScrapes.Desc()
	ch <- pe.failedScrapes.Desc()
}

// Implements prometheus.Collector
func (pe *prometheusExporter) Collect(ch chan<- prometheus.Metric) {
	if StartParams.Verbose {
		defer func(start time.Time) {
			logInfo("prometheus.Collector.Collect   %s", time.Now().Sub(start))
		}(time.Now())
	}

	pe.totalScrapes.Inc()
	pe.up.Set(1)

	// scrape
	err := VarnishExporter.Update()
	if err != nil {
		pe.up.Set(0)
		pe.failedScrapes.Inc()
	}

	// reset
	for _, pMetric := range pe.metrics {
		pMetric.Reset()
	}
	// update values
	if err == nil {
		for _, pMetric := range pe.metrics {
			if vMetric := VarnishExporter.metricsByName[pMetric.NameVarnish]; vMetric != nil {
				pMetric.Set(vMetric.Value)
			}
		}
	}
	// collect
	for _, pMetric := range pe.metrics {
		pMetric.Gauge().Collect(ch)
	}
	ch <- pe.up
	ch <- pe.totalScrapes
	ch <- pe.failedScrapes
}

func (pe *prometheusExporter) exposeMetrics(metrics []*varnishMetric) error {
	pe.metrics = make([]*prometheusMetric, 0)

	for _, m := range metrics {
		pm := NewPrometheusMetric(m)
		opts := prometheus.GaugeOpts{
			Namespace: pe.namespace,
			Name:      pm.Name,
			Help:      m.Description,
			// @todo Put varnish version number here or should it be its own metric
			//ConstLabels: prometheus.Labels{"type": pm.Group},
		}

		if labelNames := pm.LabelNames(); len(labelNames) > 0 {
			pm.gaugeVec = prometheus.NewGaugeVec(opts, labelNames)
		} else {
			pm.gauge = prometheus.NewGauge(opts)
		}
		pe.metrics = append(pe.metrics, pm)
	}
	return nil
}

// https://prometheus.io/docs/practices/naming/
func fullPrometheusMetricName(metric *varnishMetric) string {
	clean := strings.ToLower(metric.Name)
	// Remove unique identifiers from name to group similar metrics into a single GaugeVec
	if len(metric.Identifier) > 0 {
		clean = strings.Replace(clean, "."+strings.ToLower(metric.Identifier), "", -1)
	}
	// Make sure our group name is prefixed only once
	return prometheusGroup(metric) + "_" + strings.Replace(prometheusGroupTrim(clean), ".", "_", -1)
}

type group struct {
	name     string
	prefixes []string
}

var (
	groups = []group{
		// @note varnish 3.x does not seem to mark 'MAIN.' prefixes
		group{name: "backend", prefixes: []string{
			"VBE.",
			// varnish 4.x
			"MAIN.backend_",
			"MAIN.s_fetch",
			// varnish 3.x
			"backend_",
			"MAIN.s_fetch",
		}},
		group{name: "mempool", prefixes: []string{
			"MEMPOOL.",
		}},
		group{name: "lck", prefixes: []string{
			"LCK.",
		}},
		group{name: "sma", prefixes: []string{
			"SMA.",
		}},
		group{name: "mgt", prefixes: []string{
			"MGT.",
		}},
		// must be last so other groups have time to override
		group{name: "main", prefixes: []string{
			"MAIN.",
		}},
	}
)

func prometheusGroupTrim(name string) string {
	for _, group := range groups {
		for _, prefix := range group.prefixes {
			if startsWith(name, prefix, caseInsensitive) {
				return name[len(prefix):]
			}
		}
	}
	return name
}

// Always returns at least one main label
func prometheusGroup(metric *varnishMetric) string {
	for _, group := range groups {
		if startsWithAny(metric.Name, group.prefixes, caseInsensitive) {
			return group.name
		}
	}
	return "main"
}

// @note may modify input ptrs if finds a GaugeVec grouping pattern
func prometheusLabels(pMetric *prometheusMetric, metric *varnishMetric) prometheus.Labels {
	labels := make(prometheus.Labels)
	if len(metric.Identifier) > 0 {
		labels["ident"] = metric.Identifier
	}
	if startsWith(pMetric.Name, "main_fetch_", caseSensitive) {
		// If name is manipulated to be the same for multiple metrics
		// the description needs to match as well.
		labels["code"] = pMetric.Name[len("main_fetch_"):]
		pMetric.Name = "main_fetch"
		metric.Description = "Number of backend fetches"
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}
