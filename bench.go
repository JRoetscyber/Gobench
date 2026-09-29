package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

// setupStats holds new-connection costs; allocated lazily since most requests reuse connections.
type setupStats struct{ dns, tcp, tls histogram }

// stats is owned by a single worker while running (no locking) and merged at the end.
type stats struct {
	hist     histogram // total latency: request sent -> body fully read
	ttfb     histogram // request written -> first response byte
	download histogram // first response byte -> body fully read
	setup    *setupStats

	statuses map[int]int64
	errors   map[string]int64
	bytes    int64

	newConns, reusedConns int64
	satisfied, tolerating int64
	timeline              []int64 // completed requests per elapsed second
}

func newStats() *stats {
	return &stats{statuses: map[int]int64{}, errors: map[string]int64{}}
}

func (s *stats) merge(o *stats) {
	s.hist.merge(&o.hist)
	s.ttfb.merge(&o.ttfb)
	s.download.merge(&o.download)
	if o.setup != nil {
		if s.setup == nil {
			s.setup = &setupStats{}
		}
		s.setup.dns.merge(&o.setup.dns)
		s.setup.tcp.merge(&o.setup.tcp)
		s.setup.tls.merge(&o.setup.tls)
	}
	s.bytes += o.bytes
	s.newConns += o.newConns
	s.reusedConns += o.reusedConns
	s.satisfied += o.satisfied
	s.tolerating += o.tolerating
	for len(s.timeline) < len(o.timeline) {
		s.timeline = append(s.timeline, 0)
	}
	for i, v := range o.timeline {
		s.timeline[i] += v
	}
	for k, v := range o.statuses {
		s.statuses[k] += v
	}
	for k, v := range o.errors {
		s.errors[k] += v
	}
}

func (s *stats) tick(start time.Time) {
	sec := int(time.Since(start) / time.Second)
	for len(s.timeline) <= sec {
		s.timeline = append(s.timeline, 0)
	}
	s.timeline[sec]++
}

// traceTimes are the moments recorded by net/http/httptrace for one request.
type traceTimes struct {
	dnsStart, dnsDone, connStart, connDone, tlsStart, tlsDone, wrote, first time.Time
	got, reused                                                             bool
}

// reqTrace guards traceTimes: hooks can fire from the dialer goroutine.
type reqTrace struct {
	mu sync.Mutex
	t  traceTimes
}

func (r *reqTrace) mark(dst *time.Time) {
	r.mu.Lock()
	*dst = time.Now()
	r.mu.Unlock()
}

func (r *reqTrace) snapshot() traceTimes {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t
}

func (r *reqTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { r.mark(&r.t.dnsStart) },
		DNSDone:  func(httptrace.DNSDoneInfo) { r.mark(&r.t.dnsDone) },
		ConnectStart: func(_, _ string) {
			r.mu.Lock()
			if r.t.connStart.IsZero() {
				r.t.connStart = time.Now()
			}
			r.mu.Unlock()
		},
		ConnectDone:       func(_, _ string, _ error) { r.mark(&r.t.connDone) },
		TLSHandshakeStart: func() { r.mark(&r.t.tlsStart) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { r.mark(&r.t.tlsDone) },
		GotConn: func(i httptrace.GotConnInfo) {
			r.mu.Lock()
			r.t.got, r.t.reused = true, i.Reused
			r.mu.Unlock()
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { r.mark(&r.t.wrote) },
		GotFirstResponseByte: func() { r.mark(&r.t.first) },
	}
}

func span(from, to time.Time) (time.Duration, bool) {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0, false
	}
	return to.Sub(from), true
}

func (s *stats) recordTrace(t traceTimes, end time.Time) {
	if t.got {
		if t.reused {
			s.reusedConns++
		} else {
			s.newConns++
		}
	}
	if d, ok := span(t.wrote, t.first); ok {
		s.ttfb.add(d)
	}
	if d, ok := span(t.first, end); ok {
		s.download.add(d)
	}
	if t.got && !t.reused {
		dns, okD := span(t.dnsStart, t.dnsDone)
		tcp, okT := span(t.connStart, t.connDone)
		tl, okS := span(t.tlsStart, t.tlsDone)
		if okD || okT || okS {
			if s.setup == nil {
				s.setup = &setupStats{}
			}
			if okD {
				s.setup.dns.add(dns)
			}
			if okT {
				s.setup.tcp.add(tcp)
			}
			if okS {
				s.setup.tls.add(tl)
			}
		}
	}
}

// progress holds live counters for the optional progress ticker.
type progress struct{ done, failed atomic.Int64 }

func (p *progress) add(failed bool) {
	p.done.Add(1)
	if failed {
		p.failed.Add(1)
	}
}

type runResult struct {
	stats       *stats
	elapsed     time.Duration
	interrupted bool
}

func newClient(cfg *config) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = cfg.Concurrency
	tr.MaxIdleConnsPerHost = cfg.Concurrency
	tr.DisableKeepAlives = !cfg.KeepAlive
	if cfg.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in flag
	}
	c := &http.Client{Transport: tr, Timeout: cfg.Timeout}
	if cfg.NoRedirect {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

func newRequest(ctx context.Context, cfg *config) (*http.Request, error) {
	var body io.Reader
	if len(cfg.Body) > 0 {
		body = bytes.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header = cfg.Headers.Clone()
	if cfg.Host != "" {
		req.Host = cfg.Host
	}
	return req, nil
}

// run executes the benchmark. ctx cancellation (e.g. Ctrl+C) aborts in-flight
// requests and discards them; reaching -d only stops issuing new requests, so
// requests already in flight finish and are counted.
func run(ctx context.Context, cfg *config, client *http.Client, prog *progress) *runResult {
	runCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	var interval time.Duration
	if cfg.Rate > 0 {
		interval = time.Duration(float64(time.Second) / cfg.Rate)
	}

	var claimed atomic.Int64
	all := make([]*stats, cfg.Concurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for w := range all {
		st := newStats()
		all[w] = st
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if runCtx.Err() != nil {
					return
				}
				idx := claimed.Add(1) - 1
				if cfg.Requests > 0 && idx >= cfg.Requests {
					return
				}
				// Each request gets a fixed global time slot, which gives an
				// accurate overall rate regardless of worker count.
				if interval > 0 && !sleepUntil(runCtx, start.Add(time.Duration(idx)*interval)) {
					return
				}
				doRequest(ctx, cfg, client, st, prog, start)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := newStats()
	for _, s := range all {
		total.merge(s)
	}
	return &runResult{stats: total, elapsed: elapsed, interrupted: ctx.Err() != nil}
}

func doRequest(ctx context.Context, cfg *config, client *http.Client, st *stats, prog *progress, start time.Time) {
	req, err := newRequest(ctx, cfg)
	if err != nil {
		st.errors["request"]++
		st.tick(start)
		prog.add(true)
		return
	}
	tr := &reqTrace{}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), tr.hooks()))

	t0 := time.Now()
	resp, err := client.Do(req)
	var n int64
	if err == nil {
		n, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	end := time.Now()
	d := end.Sub(t0)

	if err != nil {
		if ctx.Err() != nil {
			return // aborted by the user, not a real failure
		}
		st.errors[classify(err)]++
		st.tick(start)
		prog.add(true)
		return
	}

	failed := resp.StatusCode >= 400
	st.bytes += n
	st.statuses[resp.StatusCode]++
	st.hist.add(d)
	st.recordTrace(tr.snapshot(), end)
	switch {
	case failed:
	case d <= cfg.ApdexT:
		st.satisfied++
	case d <= 4*cfg.ApdexT:
		st.tolerating++
	}
	st.tick(start)
	prog.add(failed)
}

func classify(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &certErr):
		return "tls"
	case errors.As(err, &opErr):
		return "connection"
	default:
		return "other"
	}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func reportProgress(ctx context.Context, w io.Writer, p *progress, start time.Time, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var last int64
	lastT := start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			done := p.done.Load()
			rps := float64(done-last) / now.Sub(lastT).Seconds()
			fmt.Fprintf(w, "[%4.0fs] %d done, %.0f req/s, %d failed\n",
				now.Sub(start).Seconds(), done, rps, p.failed.Load())
			last, lastT = done, now
		}
	}
}
