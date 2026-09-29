package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const version = "1.1.0"

type headerList []string

func (h *headerList) String() string { return strings.Join(*h, "; ") }

func (h *headerList) Set(v string) error {
	if name, _, ok := strings.Cut(v, ":"); !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("invalid header %q, want \"Name: value\"", v)
	}
	*h = append(*h, v)
	return nil
}

type config struct {
	URL          string
	Method       string
	Requests     int64
	Duration     time.Duration
	Concurrency  int
	Rate         float64
	Timeout      time.Duration
	Headers      http.Header
	Host         string
	Body         []byte
	KeepAlive    bool
	Insecure     bool
	NoRedirect   bool
	JSON         bool
	Progress     bool
	FailRate     float64
	ApdexT       time.Duration
	TargetRPS    float64
	Explain      bool
	Strict       bool
	GlossaryOnly bool
	Version      bool
}

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	fs := flag.NewFlagSet("gobench", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var hdrs headerList
	var (
		target       = fs.String("u", "", "target URL (or pass it as the last argument)")
		requests     = fs.Int64("n", 0, "total number of requests (default 1000 if -d is not set)")
		duration     = fs.Duration("d", 0, "run for this long, e.g. 30s (stops at -n or -d, whichever comes first)")
		conc         = fs.Int("c", 10, "number of concurrent workers")
		rate         = fs.Float64("q", 0, "max requests per second across all workers (0 = unlimited)")
		method       = fs.String("m", "GET", "HTTP method")
		body         = fs.String("body", "", "request body")
		bodyFile     = fs.String("body-file", "", "read request body from a file")
		timeout      = fs.Duration("timeout", 30*time.Second, "per-request timeout")
		keepAlive    = fs.Bool("keepalive", true, "reuse connections (-keepalive=false to disable)")
		insecure     = fs.Bool("insecure", false, "skip TLS certificate verification")
		noRedirect   = fs.Bool("no-redirect", false, "do not follow redirects")
		asJSON       = fs.Bool("json", false, "print the report as JSON")
		showProgress = fs.Bool("progress", false, "print progress to stderr every 5s")
		failRate     = fs.Float64("fail-rate", -1, "exit with status 2 if failure rate (%) exceeds this (-1 = off)")
		apdexT       = fs.Duration("apdex-t", 500*time.Millisecond, "target response time used by the verdict and Apdex score")
		targetRPS    = fs.Float64("target-rps", 0, "throughput you need; adds a check to the verdict (0 = off)")
		explain      = fs.Bool("explain", false, "explain every metric (on its own, prints just the glossary)")
		strict       = fs.Bool("strict", false, "exit with status 2 if the verdict is BAD")
		showVersion  = fs.Bool("version", false, "print version and exit")
	)
	fs.Var(&hdrs, "H", `extra header "Name: value" (repeatable; "Host: x" overrides the Host header)`)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "Usage: gobench [flags] [URL]")
			fs.SetOutput(stderr)
			fs.PrintDefaults()
		}
		return nil, err
	}
	if *showVersion {
		return &config{Version: true}, nil
	}

	raw := *target
	if raw == "" && fs.NArg() == 1 {
		raw = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if raw == "" && *explain {
		return &config{Explain: true, GlossaryOnly: true}, nil
	}
	if raw == "" {
		return nil, errors.New("a target URL is required (-u URL)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid URL %q: must be http:// or https:// with a host", raw)
	}

	m := strings.ToUpper(strings.TrimSpace(*method))
	if m == "" || strings.IndexFunc(m, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
		return nil, fmt.Errorf("invalid method %q", *method)
	}

	switch {
	case *requests < 0:
		return nil, errors.New("-n must not be negative")
	case *duration < 0:
		return nil, errors.New("-d must not be negative")
	case *conc < 1:
		return nil, errors.New("-c must be at least 1")
	case *rate < 0:
		return nil, errors.New("-q must not be negative")
	case *timeout <= 0:
		return nil, errors.New("-timeout must be positive")
	case *apdexT <= 0:
		return nil, errors.New("-apdex-t must be positive")
	case *targetRPS < 0:
		return nil, errors.New("-target-rps must not be negative")
	case *body != "" && *bodyFile != "":
		return nil, errors.New("use either -body or -body-file, not both")
	}

	if *requests == 0 && *duration == 0 {
		*requests = 1000
	}
	if *requests > 0 && int64(*conc) > *requests {
		*conc = int(*requests)
	}

	var payload []byte
	switch {
	case *bodyFile != "":
		if payload, err = os.ReadFile(*bodyFile); err != nil {
			return nil, fmt.Errorf("reading -body-file: %w", err)
		}
	case *body != "":
		payload = []byte(*body)
	}

	cfg := &config{
		URL:         u.String(),
		Method:      m,
		Requests:    *requests,
		Duration:    *duration,
		Concurrency: *conc,
		Rate:        *rate,
		Timeout:     *timeout,
		Headers:     make(http.Header),
		Body:        payload,
		KeepAlive:   *keepAlive,
		Insecure:    *insecure,
		NoRedirect:  *noRedirect,
		JSON:        *asJSON,
		Progress:    *showProgress,
		FailRate:    *failRate,
		ApdexT:      *apdexT,
		TargetRPS:   *targetRPS,
		Explain:     *explain,
		Strict:      *strict,
	}
	for _, h := range hdrs {
		name, value, _ := strings.Cut(h, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if strings.EqualFold(name, "Host") {
			cfg.Host = value
		} else {
			cfg.Headers.Add(name, value)
		}
	}
	if cfg.Headers.Get("User-Agent") == "" {
		cfg.Headers.Set("User-Agent", "gobench/"+version)
	}
	return cfg, nil
}
