package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ProposalCount is how many DriftProposals a policy has in a phase.
type ProposalCount struct {
	Policy Policy
	Phase  string
	Count  int
}

// proposalsCollector reports the proposals by policy and phase on every scrape,
// from what list returns (the manager's cache), so the gauge is never out of
// date and a policy or a phase that is gone has no series.
type proposalsCollector struct {
	list func() []ProposalCount
	desc *prometheus.Desc
}

// NewProposalsCollector returns the collector of backflow_proposals. list is
// called on every scrape and must be fast: it reads from a cache.
func NewProposalsCollector(list func() []ProposalCount) prometheus.Collector {
	return &proposalsCollector{
		list: list,
		desc: prometheus.NewDesc(namespace+"_proposals",
			"DriftProposals by policy and phase, as they are now.", []string{lPolicyNS, lPolicy, "phase"}, nil),
	}
}

// RegisterProposals registers the collector on the registry of the manager. It
// is meant to be called once, at start.
func RegisterProposals(list func() []ProposalCount) error {
	return Registry.Register(NewProposalsCollector(list))
}

func (c *proposalsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *proposalsCollector) Collect(ch chan<- prometheus.Metric) {
	defer func() { _ = recover() }() // a metric never fails a scrape
	for _, p := range c.list() {
		ns, n := p.Policy.labels()
		m, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, float64(p.Count), ns, n, bound("phase", p.Phase))
		if err == nil {
			ch <- m
		}
	}
}
