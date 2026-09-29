package main

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustConfig(t *testing.T, args ...string) *config {
	t.Helper()
	cfg, err := parseFlags(args, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(%v): %v", args, err)
	}
	return cfg
}

func runCfg(ctx context.Context, cfg *config) *runResult {
	client := newClient(cfg)
	defer client.CloseIdleConnections()
	return run(ctx, cfg, client, &progress{})
}

func TestRunCountsAllRequests(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-n", "200", "-c", "10"))
	if got := hits.Load(); got != 200 {
		t.Fatalf("server saw %d requests, want 200", got)
	}
	if res.stats.statuses[200] != 200 || res.stats.hist.total != 200 {
		t.Fatalf("stats: statuses=%v total=%d", res.stats.statuses, res.stats.hist.total)
	}
	if res.stats.bytes != 400 {
		t.Fatalf("bytes = %d, want 400", res.stats.bytes)
	}
}

func TestFailuresCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := mustConfig(t, "-u", srv.URL+"/", "-n", "50", "-c", "5")
	rep := buildReport(cfg, runCfg(context.Background(), cfg))
	if rep.Total != 50 || rep.Failed != 50 || rep.FailedPercent != 100 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestRequestShape(t *testing.T) {
	type seen struct{ method, header, host, body string }
	ch := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- seen{r.Method, r.Header.Get("X-Test"), r.Host, string(b)}
	}))
	defer srv.Close()

	runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-m", "post",
		"-H", "X-Test: abc", "-H", "Host: example.test", "-body", "hello", "-n", "1", "-c", "1"))
	got := <-ch
	want := seen{"POST", "abc", "example.test", "hello"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	// 21 requests at 100/s: the last is scheduled 200ms after the first.
	res := runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-q", "100", "-n", "21", "-c", "5"))
	if res.elapsed < 190*time.Millisecond {
		t.Fatalf("elapsed %v, rate limit not applied", res.elapsed)
	}
}

func TestDurationMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
	}))
	defer srv.Close()

	res := runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-d", "300ms", "-c", "4"))
	if res.elapsed < 300*time.Millisecond || res.elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v", res.elapsed)
	}
	if res.stats.hist.total == 0 {
		t.Fatal("no requests completed")
	}
}

func TestConnectionErrorsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	res := runCfg(context.Background(), mustConfig(t, "-u", url+"/", "-n", "5", "-c", "1"))
	if res.stats.errors["connection"] != 5 {
		t.Fatalf("errors = %v", res.stats.errors)
	}
}

func TestTimeoutClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	res := runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-n", "2", "-c", "2", "-timeout", "100ms"))
	if res.stats.errors["timeout"] != 2 {
		t.Fatalf("errors = %v", res.stats.errors)
	}
}

func TestInterruptDiscardsInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	res := runCfg(ctx, mustConfig(t, "-u", srv.URL+"/", "-n", "10", "-c", "2"))
	if !res.interrupted || len(res.stats.errors) != 0 || res.stats.hist.total != 0 {
		t.Fatalf("interrupted=%v errors=%v total=%d", res.interrupted, res.stats.errors, res.stats.hist.total)
	}
}

func TestHistogram(t *testing.T) {
	var h histogram
	for i := 1; i <= 100; i++ {
		h.add(time.Duration(i) * time.Millisecond)
	}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{{0.50, 50 * time.Millisecond}, {0.95, 95 * time.Millisecond}, {0.99, 99 * time.Millisecond}} {
		got := h.percentile(c.p)
		if math.Abs(float64(got-c.want))/float64(c.want) > 0.03 {
			t.Errorf("p%.0f = %v, want ~%v", c.p*100, got, c.want)
		}
	}
	if h.min != time.Millisecond || h.max != 100*time.Millisecond {
		t.Errorf("min/max = %v/%v", h.min, h.max)
	}

	var a, b histogram
	a.add(time.Millisecond)
	b.add(9 * time.Millisecond)
	a.merge(&b)
	if a.total != 2 || a.max != 9*time.Millisecond || a.mean() != 5*time.Millisecond {
		t.Errorf("merge: total=%d max=%v mean=%v", a.total, a.max, a.mean())
	}
}

func TestParseFlagsValidation(t *testing.T) {
	const u = "http://example.test/"
	tests := map[string][]string{
		"no url":             nil,
		"bad scheme":         {"-u", "ftp://x/"},
		"no host":            {"-u", "http://"},
		"zero concurrency":   {"-u", u, "-c", "0"},
		"negative n":         {"-u", u, "-n", "-5"},
		"negative rate":      {"-u", u, "-q", "-1"},
		"zero timeout":       {"-u", u, "-timeout", "0s"},
		"bad method":         {"-u", u, "-m", "GE T"},
		"bad header":         {"-u", u, "-H", "nocolon"},
		"body and body-file": {"-u", u, "-body", "a", "-body-file", "x"},
		"extra args":         {"-u", u, "extra"},
	}
	for name, args := range tests {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	cfg := mustConfig(t, "http://localhost:8080/")
	if cfg.Requests != 1000 || cfg.Concurrency != 10 || !cfg.KeepAlive {
		t.Errorf("defaults: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.Headers.Get("User-Agent"), "gobench/") {
		t.Errorf("User-Agent = %q", cfg.Headers.Get("User-Agent"))
	}
	if c := mustConfig(t, "-u", "http://x/", "-n", "3", "-c", "50"); c.Concurrency != 3 {
		t.Errorf("concurrency should be capped at -n, got %d", c.Concurrency)
	}
}

func TestTraceMetricsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := runCfg(context.Background(), mustConfig(t, "-u", srv.URL+"/", "-n", "20", "-c", "2"))
	s := res.stats
	if s.ttfb.total != 20 || s.download.total != 20 {
		t.Fatalf("ttfb=%d download=%d, want 20 each", s.ttfb.total, s.download.total)
	}
	if s.newConns < 1 || s.reusedConns < 1 || s.newConns+s.reusedConns != 20 {
		t.Fatalf("conns new=%d reused=%d", s.newConns, s.reusedConns)
	}
	if s.setup == nil || s.setup.tcp.total < 1 {
		t.Fatal("expected TCP connect timings for new connections")
	}
	if got := s.ttfb.percentile(0.5); got < 4*time.Millisecond {
		t.Fatalf("median TTFB %v should include the 5ms handler delay", got)
	}
	var sum int64
	for _, v := range s.timeline {
		sum += v
	}
	if sum != 20 {
		t.Fatalf("timeline sums to %d, want 20", sum)
	}
}

func TestHistogramBinsAndStddev(t *testing.T) {
	var h histogram
	for i := 1; i <= 100; i++ {
		h.add(time.Duration(i) * time.Millisecond)
	}
	var total uint64
	for _, b := range h.bins(10) {
		total += b.count
	}
	if total != 100 {
		t.Fatalf("bins hold %d samples, want 100", total)
	}
	// stddev of 1..100 is ~28.9
	if sd := h.stddev(); sd < 28*time.Millisecond || sd > 30*time.Millisecond {
		t.Fatalf("stddev = %v", sd)
	}
}

func testCfg() *config {
	return &config{Concurrency: 50, KeepAlive: true, ApdexT: 500 * time.Millisecond}
}

func healthyReport() *report {
	return &report{
		Concurrency: 50, Total: 1000, Responses: 1000, RequestsPerSec: 200,
		StatusCodes: map[int]int64{200: 1000}, StatusClasses: map[string]int64{"2xx": 1000},
		Errors:   map[string]int64{},
		Latency:  latencyReport{MinMS: 180, P50MS: 195, P95MS: 250, P99MS: 300},
		Waiting:  latencyReport{P95MS: 200},
		ApdexTMS: 500, ApdexSatisfied: 1000, Apdex: 1,
		NewConnections: 50, ReusedConnections: 950,
	}
}

func findCheck(t *testing.T, v *verdict, name string) check {
	t.Helper()
	for _, c := range v.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q", name)
	return check{}
}

func TestVerdictHealthy(t *testing.T) {
	v := evaluate(testCfg(), healthyReport())
	if v.Overall != levelGood {
		t.Fatalf("overall = %v, checks = %+v", v.Overall, v.Checks)
	}
}

func TestVerdictSaturated(t *testing.T) {
	r := healthyReport()
	r.Failed, r.FailedPercent = 100, 10
	r.StatusCodes = map[int]int64{200: 900, 500: 100}
	r.StatusClasses = map[string]int64{"2xx": 900, "5xx": 100}
	r.Latency = latencyReport{MinMS: 180, P50MS: 1500, P95MS: 4000, P99MS: 5000}
	r.Apdex, r.ApdexSatisfied, r.ApdexTolerating = 0.3, 200, 200

	v := evaluate(testCfg(), r)
	if v.Overall != levelBad {
		t.Fatalf("overall = %v", v.Overall)
	}
	for _, name := range []string{"Reliability", "Queueing", "Speed", "Satisfaction"} {
		if c := findCheck(t, v, name); c.Level != levelBad || c.Finding == "" {
			t.Errorf("%s: level=%v finding=%q", name, c.Level, c.Finding)
		}
	}
	if !strings.Contains(v.Summary, "saturation") {
		t.Errorf("summary should diagnose saturation: %q", v.Summary)
	}
	if !strings.Contains(findCheck(t, v, "Reliability").Finding, "5xx") {
		t.Error("reliability finding should mention 5xx")
	}
}

func TestVerdictTargetRate(t *testing.T) {
	cfg := testCfg()
	cfg.TargetRPS = 1000 // healthy report does 200 req/s
	if c := findCheck(t, evaluate(cfg, healthyReport()), "Target rate"); c.Level != levelBad {
		t.Fatalf("target check level = %v", c.Level)
	}
}

func TestVerdictStability(t *testing.T) {
	r := healthyReport()
	r.Timeline = []int64{100, 200, 200, 200, 200, 100, 100, 100, 90}
	if c := findCheck(t, evaluate(testCfg(), r), "Stability"); c.Level < levelWarn {
		t.Fatalf("stability level = %v (%s)", c.Level, c.Value)
	}
}

func TestGlossaryOnlyAndASCII(t *testing.T) {
	var out, errb strings.Builder
	if code := realMain([]string{"-explain"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "HOW TO READ THIS REPORT") {
		t.Fatal("glossary not printed")
	}
	assertASCII(t, "glossary", out.String())
}

func assertASCII(t *testing.T, what, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			t.Fatalf("%s contains non-ASCII byte at %d (breaks some Windows consoles)", what, i)
		}
	}
}

func TestTextReportIsASCII(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	defer srv.Close()
	cfg := mustConfig(t, "-u", srv.URL+"/", "-n", "300", "-c", "5", "-explain")
	rep := buildReport(cfg, runCfg(context.Background(), cfg))
	var b strings.Builder
	printText(&b, rep, true)
	assertASCII(t, "report", b.String())
	if !strings.Contains(b.String(), "VERDICT:") {
		t.Fatal("report has no verdict")
	}
}
