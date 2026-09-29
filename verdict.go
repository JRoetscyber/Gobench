package main

import (
	"fmt"
	"strings"
)

const minSamples = 100

type level int

const (
	levelGood level = iota
	levelOK
	levelWarn
	levelBad
)

func (l level) String() string { return [...]string{"GOOD", "OK", "WARN", "BAD"}[l] }

func (l level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

type check struct {
	Name    string `json:"name"`
	Level   level  `json:"level"`
	Value   string `json:"value"`
	Finding string `json:"finding,omitempty"`
	Meaning string `json:"meaning"`
}

type verdict struct {
	Overall level   `json:"overall"`
	Summary string  `json:"summary"`
	Checks  []check `json:"checks"`
}

// lowerBetter grades a value where smaller is better.
func lowerBetter(v, good, ok, warn float64) level {
	switch {
	case v <= good:
		return levelGood
	case v <= ok:
		return levelOK
	case v <= warn:
		return levelWarn
	}
	return levelBad
}

func evaluate(cfg *config, r *report) *verdict {
	v := &verdict{Checks: []check{}}
	add := func(c check) {
		v.Checks = append(v.Checks, c)
		if c.Level > v.Overall {
			v.Overall = c.Level
		}
	}

	rel := checkReliability(r)
	add(rel)
	queue := levelGood
	if r.Responses > 0 {
		add(checkSpeed(r))
		if r.Responses >= minSamples {
			add(checkConsistency(r))
		}
		if c := checkQueueing(r); c != nil {
			add(*c)
			queue = c.Level
		}
		if c := checkStability(r); c != nil {
			add(*c)
		}
		if c := checkConnections(cfg, r); c != nil {
			add(*c)
		}
		add(checkApdex(r))
	}
	if cfg.TargetRPS > 0 {
		add(checkTarget(cfg, r))
	}
	v.Summary = summarize(v.Overall, rel.Level, queue)
	return v
}

func summarize(overall, rel, queue level) string {
	var s string
	switch {
	case overall == levelGood:
		s = "Healthy at this load: fast, consistent and error-free. Raise -c step by step to find where it starts to degrade."
	case queue >= levelWarn && rel >= levelWarn:
		s = "Looks like saturation: requests are queueing AND errors appear, so the site is past its comfortable capacity at this concurrency. Lower -c until Queueing and Reliability turn GOOD; that is roughly what it handles comfortably."
	case queue >= levelWarn:
		s = "Requests are queueing: the site still answers, but latency is well above its best case. You are at or near its capacity at this concurrency."
	case rel >= levelWarn:
		s = "Too many requests failed. See the Reliability finding for the likely cause."
	case overall == levelOK:
		s = "Mostly healthy, with minor concerns in the lines marked OK below."
	default:
		s = "Some checks need attention, see the flagged lines below."
	}
	if overall >= levelWarn {
		s += " Before trusting a bad result, rule out your own PC and network: repeat the run, try a lower -c, or test from another machine."
	}
	return s
}

func checkReliability(r *report) check {
	c := check{
		Name:    "Reliability",
		Value:   fmt.Sprintf("%.2f%% failed (%d of %d)", r.FailedPercent, r.Failed, r.Total),
		Meaning: "The share of requests that failed: connection errors, timeouts, or HTTP status 400 and above. A healthy site sits at or very near 0% at the load you expect.",
	}
	switch {
	case r.FailedPercent == 0:
		c.Level = levelGood
	case r.FailedPercent <= 0.1:
		c.Level = levelOK
	case r.FailedPercent <= 1:
		c.Level = levelWarn
	default:
		c.Level = levelBad
	}
	if c.Level == levelGood {
		return c
	}

	var parts []string
	if n := r.StatusClasses["5xx"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d responses were HTTP 5xx (server errors): the server accepted the request but failed to handle it, a classic sign of overload or a bug.", n))
	}
	if n := r.StatusCodes[429] + r.StatusCodes[403]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d responses were 429/403: a rate limiter, WAF or CDN is blocking you, so this measures the protection rather than your server.", n))
	}
	if n := r.StatusClasses["4xx"] - r.StatusCodes[429] - r.StatusCodes[403]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d responses were HTTP 4xx: check the URL, headers and request body.", n))
	}
	for _, e := range []struct{ kind, text string }{
		{"timeout", "requests timed out (no answer within -timeout): the server or network was too slow or overloaded."},
		{"connection", "connections were refused or reset: the server, a firewall or a connection limit turned them away."},
		{"dns", "DNS lookups failed."},
		{"tls", "TLS/certificate errors (try -insecure only for testing your own dev setup)."},
		{"other", "requests failed for other reasons."},
	} {
		if n := r.Errors[e.kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, e.text))
		}
	}
	c.Finding = strings.Join(parts, " ")
	return c
}

func checkSpeed(r *report) check {
	t := r.ApdexTMS
	p95 := r.Latency.P95MS
	c := check{
		Name:    "Speed",
		Level:   lowerBetter(p95, t, 2*t, 4*t),
		Value:   fmt.Sprintf("p95 = %.0f ms (target %.0f ms)", p95, t),
		Meaning: "How long the slowest 1 in 20 requests took (p95). Judged against your target from -apdex-t: within the target is GOOD, up to 2x OK, up to 4x WARN, beyond that BAD.",
	}
	if c.Level != levelGood {
		c.Finding = fmt.Sprintf("1 in 20 requests took longer than %.0f ms. ", p95)
		if r.Waiting.P95MS >= 0.7*p95 {
			c.Finding += "Most of that is waiting for the server (which includes one network round trip), so look at backend or database work, or queueing under load."
		} else {
			c.Finding += "Most of that is NOT server waiting: look at connection setup (DNS/TCP/TLS), large responses, or network distance. If the target is unrealistic for this kind of page, adjust -apdex-t."
		}
	}
	return c
}

func checkConsistency(r *report) check {
	c := check{
		Name:    "Consistency",
		Meaning: "The tail ratio p99 / p50: how much slower the slowest 1% is than the typical request. Near 1-3 is steady; above 10 means unpredictable slow spikes.",
	}
	if r.Latency.P50MS <= 0 {
		c.Value = "n/a"
		return c
	}
	ratio := r.Latency.P99MS / r.Latency.P50MS
	c.Level = lowerBetter(ratio, 3, 6, 10)
	c.Value = fmt.Sprintf("p99 is %.1fx the median (%.0f ms vs %.0f ms)", ratio, r.Latency.P99MS, r.Latency.P50MS)
	if c.Level != levelGood {
		c.Finding = "Some requests are much slower than typical. Common causes: garbage collection or lock contention, slow database queries, cold caches, retries, or network hiccups. If it only shows up at high concurrency it is usually queueing. A few slow requests at the very start (connection setup) are normal."
	}
	return c
}

func checkQueueing(r *report) *check {
	if r.Responses < 20 || r.Latency.MinMS <= 0 || r.Latency.P50MS <= 0 {
		return nil
	}
	ratio := r.Latency.P50MS / r.Latency.MinMS
	c := &check{
		Name:    "Queueing",
		Level:   lowerBetter(ratio, 1.5, 3, 6),
		Value:   fmt.Sprintf("median is %.1fx the best case (%.0f ms vs %.0f ms)", ratio, r.Latency.P50MS, r.Latency.MinMS),
		Meaning: "The median (p50) divided by the fastest request (min). The fastest request is about what a lone request costs, so a big gap means requests are waiting in line.",
	}
	if c.Level != levelGood {
		c.Finding = fmt.Sprintf("The typical request is far slower than the best case, which means requests are waiting in line: the server (or a proxy, CDN or database in front of it) is saturated at concurrency %d. Lower -c until this turns GOOD to find the load it handles comfortably.", r.Concurrency)
	}
	return c
}

// checkStability compares throughput late in the run to early in the run.
func checkStability(r *report) *check {
	if r.Responses < minSamples || len(r.Timeline) < 6 {
		return nil
	}
	inner := r.Timeline[1:] // skip the first second: connections are still warming up
	k := len(inner) / 3
	avg := func(xs []int64) float64 {
		var s int64
		for _, x := range xs {
			s += x
		}
		return float64(s) / float64(len(xs))
	}
	early, late := avg(inner[:k]), avg(inner[len(inner)-k:])
	if early <= 0 {
		return nil
	}
	ratio := late / early
	c := &check{
		Name:    "Stability",
		Value:   fmt.Sprintf("late throughput is %.0f%% of early throughput", ratio*100),
		Meaning: "Throughput in the last third of the run compared with the first third. A steady site stays flat.",
	}
	switch {
	case ratio >= 0.9:
		c.Level = levelGood
	case ratio >= 0.75:
		c.Level = levelOK
	case ratio >= 0.5:
		c.Level = levelWarn
	default:
		c.Level = levelBad
	}
	if c.Level != levelGood {
		c.Finding = "Throughput fell while the test ran. Possible causes: memory growth or leaks, exhausted connection pools, caches filling or being evicted, thermal throttling, or a rate limiter kicking in."
	}
	return c
}

func checkConnections(cfg *config, r *report) *check {
	total := r.NewConnections + r.ReusedConnections
	if !cfg.KeepAlive || total < 50 {
		return nil
	}
	extra := r.NewConnections - int64(r.Concurrency)
	if extra < 0 {
		extra = 0
	}
	pct := float64(extra) / float64(total) * 100
	c := &check{
		Name:    "Connections",
		Level:   lowerBetter(pct, 2, 10, 30),
		Value:   fmt.Sprintf("%d new connections for %d requests (about %d expected)", r.NewConnections, total, r.Concurrency),
		Meaning: "Keep-alive lets each worker reuse one connection. Many more new connections than workers means the handshake cost is paid over and over.",
	}
	if c.Level != levelGood {
		c.Finding = "More new connections than needed: the server, a load balancer or a CDN is closing keep-alive connections early (or connections are failing and being retried). Each one costs a TCP and TLS handshake, visible under 'New conn' above."
	}
	return c
}

func apdexRating(a float64) (string, level) {
	switch {
	case a >= 0.94:
		return "Excellent", levelGood
	case a >= 0.85:
		return "Good", levelOK
	case a >= 0.70:
		return "Fair", levelWarn
	case a >= 0.50:
		return "Poor", levelBad
	}
	return "Unacceptable", levelBad
}

func checkApdex(r *report) check {
	name, lvl := apdexRating(r.Apdex)
	c := check{
		Name:    "Satisfaction",
		Level:   lvl,
		Value:   fmt.Sprintf("Apdex %.2f (%s)", r.Apdex, name),
		Meaning: "Apdex from 0 to 1. Requests within the target (-apdex-t) count fully, up to 4x the target count half, slower or failed count zero. 0.94+ excellent, 0.85+ good, 0.70+ fair, below poor.",
	}
	if lvl != levelGood && r.Total > 0 {
		t := float64(r.Total)
		c.Finding = fmt.Sprintf("%.0f%% of requests were satisfied (within %.0f ms), %.0f%% tolerable (up to %.0f ms); the rest were slower or failed.",
			float64(r.ApdexSatisfied)/t*100, r.ApdexTMS, float64(r.ApdexTolerating)/t*100, 4*r.ApdexTMS)
	}
	return c
}

func checkTarget(cfg *config, r *report) check {
	ratio := r.RequestsPerSec / cfg.TargetRPS
	c := check{
		Name:    "Target rate",
		Value:   fmt.Sprintf("%.1f req/s vs target %.1f", r.RequestsPerSec, cfg.TargetRPS),
		Meaning: "Your measured throughput compared with the -target-rps you said you need.",
	}
	switch {
	case ratio >= 1:
		c.Level = levelGood
	case ratio >= 0.8:
		c.Level = levelOK
	case ratio >= 0.5:
		c.Level = levelWarn
	default:
		c.Level = levelBad
	}
	if c.Level != levelGood {
		c.Finding = "Below your target. Throughput is capped at concurrency / mean latency, so if latency and errors look fine, raise -c. If Queueing or Reliability are flagged, the server itself is the limit."
	}
	return c
}
