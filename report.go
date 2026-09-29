package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

type latencyReport struct {
	MinMS    float64 `json:"min"`
	MeanMS   float64 `json:"mean"`
	StdDevMS float64 `json:"stddev"`
	P50MS    float64 `json:"p50"`
	P75MS    float64 `json:"p75"`
	P90MS    float64 `json:"p90"`
	P95MS    float64 `json:"p95"`
	P99MS    float64 `json:"p99"`
	P999MS   float64 `json:"p99_9"`
	MaxMS    float64 `json:"max"`
}

type connectReport struct {
	Measured int64          `json:"new_connections_measured"`
	DNS      *latencyReport `json:"dns,omitempty"`
	TCP      *latencyReport `json:"tcp,omitempty"`
	TLS      *latencyReport `json:"tls,omitempty"`
}

type binReport struct {
	FromMS  float64 `json:"from_ms"`
	ToMS    float64 `json:"to_ms"`
	Count   int64   `json:"count"`
	Percent float64 `json:"percent"`
}

type report struct {
	URL               string           `json:"url"`
	Method            string           `json:"method"`
	Concurrency       int              `json:"concurrency"`
	Total             int64            `json:"total_requests"`
	Responses         int64            `json:"responses_received"`
	Failed            int64            `json:"failed_requests"`
	FailedPercent     float64          `json:"failed_percent"`
	ElapsedSeconds    float64          `json:"elapsed_seconds"`
	RequestsPerSec    float64          `json:"requests_per_second"`
	BytesReceived     int64            `json:"bytes_received"`
	AvgResponseBytes  int64            `json:"avg_response_bytes"`
	MBPerSec          float64          `json:"megabytes_per_second"`
	StatusCodes       map[int]int64    `json:"status_codes"`
	StatusClasses     map[string]int64 `json:"status_classes"`
	Errors            map[string]int64 `json:"errors"`
	NewConnections    int64            `json:"new_connections"`
	ReusedConnections int64            `json:"reused_connections"`
	Latency           latencyReport    `json:"latency_ms"`
	Waiting           latencyReport    `json:"waiting_ttfb_ms"`
	Download          latencyReport    `json:"download_ms"`
	Connect           *connectReport   `json:"connect_ms,omitempty"`
	Distribution      []binReport      `json:"latency_distribution"`
	Timeline          []int64          `json:"completed_per_second"`
	Apdex             float64          `json:"apdex"`
	ApdexTMS          float64          `json:"apdex_t_ms"`
	ApdexSatisfied    int64            `json:"apdex_satisfied"`
	ApdexTolerating   int64            `json:"apdex_tolerating"`
	Interrupted       bool             `json:"interrupted"`
	Verdict           *verdict         `json:"verdict"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func latencyOf(h *histogram) latencyReport {
	return latencyReport{
		MinMS: ms(h.min), MeanMS: ms(h.mean()), StdDevMS: ms(h.stddev()),
		P50MS: ms(h.percentile(0.50)), P75MS: ms(h.percentile(0.75)),
		P90MS: ms(h.percentile(0.90)), P95MS: ms(h.percentile(0.95)),
		P99MS: ms(h.percentile(0.99)), P999MS: ms(h.percentile(0.999)),
		MaxMS: ms(h.max),
	}
}

func optLatency(h *histogram) *latencyReport {
	if h.total == 0 {
		return nil
	}
	l := latencyOf(h)
	return &l
}

// buildReport treats transport errors and HTTP status >= 400 as failures.
func buildReport(cfg *config, res *runResult) *report {
	s := res.stats
	r := &report{
		URL: cfg.URL, Method: cfg.Method, Concurrency: cfg.Concurrency,
		ElapsedSeconds: res.elapsed.Seconds(), BytesReceived: s.bytes,
		StatusCodes: s.statuses, Errors: s.errors, Interrupted: res.interrupted,
		StatusClasses: map[string]int64{}, Responses: int64(s.hist.total),
		NewConnections: s.newConns, ReusedConnections: s.reusedConns,
		ApdexTMS: ms(cfg.ApdexT), ApdexSatisfied: s.satisfied, ApdexTolerating: s.tolerating,
	}
	for code, n := range s.statuses {
		r.Total += n
		r.StatusClasses[fmt.Sprintf("%dxx", code/100)] += n
		if code >= 400 {
			r.Failed += n
		}
	}
	for _, n := range s.errors {
		r.Total += n
		r.Failed += n
	}
	if r.Total > 0 {
		r.FailedPercent = float64(r.Failed) / float64(r.Total) * 100
		r.Apdex = (float64(s.satisfied) + float64(s.tolerating)/2) / float64(r.Total)
	}
	if r.ElapsedSeconds > 0 {
		r.RequestsPerSec = float64(r.Total) / r.ElapsedSeconds
		r.MBPerSec = float64(s.bytes) / 1e6 / r.ElapsedSeconds
	}
	if s.hist.total > 0 {
		r.AvgResponseBytes = s.bytes / int64(s.hist.total)
	}

	r.Latency = latencyOf(&s.hist)
	r.Waiting = latencyOf(&s.ttfb)
	r.Download = latencyOf(&s.download)
	if s.setup != nil {
		r.Connect = &connectReport{
			Measured: int64(s.setup.tcp.total),
			DNS:      optLatency(&s.setup.dns),
			TCP:      optLatency(&s.setup.tcp),
			TLS:      optLatency(&s.setup.tls),
		}
	}
	for _, b := range s.hist.bins(10) {
		r.Distribution = append(r.Distribution, binReport{
			FromMS: ms(b.lo), ToMS: ms(b.hi), Count: int64(b.count),
			Percent: float64(b.count) / float64(s.hist.total) * 100,
		})
	}
	// Only whole seconds: the last, partial second would understate throughput.
	if full := int(res.elapsed / time.Second); full > 0 {
		r.Timeline = make([]int64, full)
		copy(r.Timeline, s.timeline)
	}

	r.Verdict = evaluate(cfg, r)
	return r
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func bar(v, max float64, width int) string {
	if max <= 0 || v <= 0 {
		return ""
	}
	n := int(math.Round(v / max * float64(width)))
	if n < 1 {
		n = 1
	}
	return strings.Repeat("#", n)
}

// wrap prints text word-wrapped; first/rest are the prefixes of the first and following lines.
func wrap(w io.Writer, text, first, rest string, width int) {
	line, empty := first, true
	for _, word := range strings.Fields(text) {
		switch {
		case empty:
			line += word
			empty = false
		case len(line)+1+len(word) > width:
			fmt.Fprintln(w, line)
			line = rest + word
		default:
			line += " " + word
		}
	}
	if !empty {
		fmt.Fprintln(w, line)
	}
}

// printText output is plain ASCII so it renders in any Windows console code page.
func printText(w io.Writer, r *report, explain bool) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	p("Target:        %s %s\n", r.Method, r.URL)
	p("Concurrency:   %d\n", r.Concurrency)
	note := ""
	if r.Interrupted {
		note = " (interrupted)"
	}
	p("Elapsed:       %.2fs%s\n", r.ElapsedSeconds, note)
	p("Requests:      %d total, %d failed (%.2f%%)\n", r.Total, r.Failed, r.FailedPercent)
	p("Throughput:    %.1f req/s, %.2f MB/s", r.RequestsPerSec, r.MBPerSec)
	if r.AvgResponseBytes > 0 {
		p(" (avg response %s)", humanBytes(r.AvgResponseBytes))
	}
	p("\n")
	if r.NewConnections+r.ReusedConnections > 0 {
		all := float64(r.NewConnections + r.ReusedConnections)
		p("Connections:   %d new, %d reused (%.1f%% reuse)\n",
			r.NewConnections, r.ReusedConnections, float64(r.ReusedConnections)/all*100)
	}

	codes := make([]int, 0, len(r.StatusCodes))
	for c := range r.StatusCodes {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		p("  HTTP %d:      %d\n", c, r.StatusCodes[c])
	}
	kinds := make([]string, 0, len(r.Errors))
	for k := range r.Errors {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		p("  error %s:  %d\n", k, r.Errors[k])
	}

	if r.Responses > 0 {
		l := r.Latency
		p("\nLatency (ms, request sent -> full response received):\n")
		p("  min %.1f | mean %.1f | stddev %.1f | max %.1f\n", l.MinMS, l.MeanMS, l.StdDevMS, l.MaxMS)
		p("  p50 %.1f | p75 %.1f | p90 %.1f | p95 %.1f | p99 %.1f | p99.9 %.1f\n",
			l.P50MS, l.P75MS, l.P90MS, l.P95MS, l.P99MS, l.P999MS)

		p("\nWhere the time goes (ms, p50 / p95 / max):\n")
		phase := func(name string, l *latencyReport) {
			if l != nil {
				p("  %-26s %8.1f / %8.1f / %8.1f\n", name, l.P50MS, l.P95MS, l.MaxMS)
			}
		}
		phase("Waiting for server (TTFB)", &r.Waiting)
		phase("Download", &r.Download)
		if r.Connect != nil {
			phase("New conn: DNS lookup", r.Connect.DNS)
			phase("New conn: TCP connect", r.Connect.TCP)
			phase("New conn: TLS handshake", r.Connect.TLS)
			p("  (connection setup measured on %d new connections)\n", r.Connect.Measured)
		}

		if len(r.Distribution) > 1 {
			p("\nLatency distribution (log-spaced ranges, ms):\n")
			maxPct := 0.0
			for _, b := range r.Distribution {
				if b.Percent > maxPct {
					maxPct = b.Percent
				}
			}
			for _, b := range r.Distribution {
				p("  %8.1f - %8.1f  %-30s %5.1f%%\n", b.FromMS, b.ToMS, bar(b.Percent, maxPct, 30), b.Percent)
			}
		}
	}

	if len(r.Timeline) >= 3 {
		groups := len(r.Timeline)
		if groups > 10 {
			groups = 10
		}
		avgs, labels := make([]float64, groups), make([]string, groups)
		maxAvg := 0.0
		for g := 0; g < groups; g++ {
			lo, hi := g*len(r.Timeline)/groups, (g+1)*len(r.Timeline)/groups
			var sum int64
			for _, v := range r.Timeline[lo:hi] {
				sum += v
			}
			avgs[g] = float64(sum) / float64(hi-lo)
			labels[g] = fmt.Sprintf("%3d-%3ds", lo, hi)
			if avgs[g] > maxAvg {
				maxAvg = avgs[g]
			}
		}
		p("\nThroughput over time (completed requests per second):\n")
		for g := range avgs {
			p("  %s  %-30s %8.1f\n", labels[g], bar(avgs[g], maxAvg, 30), avgs[g])
		}
	}

	printVerdict(w, r.Verdict, explain)
}

func printVerdict(w io.Writer, v *verdict, explain bool) {
	fmt.Fprintf(w, "\nVERDICT: %s\n", v.Overall)
	wrap(w, v.Summary, "  ", "  ", 96)
	fmt.Fprintln(w)
	for _, c := range v.Checks {
		fmt.Fprintf(w, "  [%-4s] %-12s %s\n", c.Level, c.Name, c.Value)
		if c.Level != levelGood && c.Finding != "" {
			wrap(w, c.Finding, "           -> ", "              ", 100)
		}
		if explain {
			wrap(w, "What it measures: "+c.Meaning, "           ", "           ", 100)
		}
	}
}
