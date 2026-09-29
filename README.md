# gobench

A lightweight, zero-dependency HTTP load tester written in Go with built-in automated verdict analysis, sub-millisecond latency distribution tracking, and CI/CD quality gate enforcement.

---

## Overview

gobench is an ApacheBench-style benchmarking CLI designed for engineering teams that need immediate, actionable feedback on backend HTTP performance. Unlike traditional benchmark tools that output raw numbers and leave interpretation to guesswork, gobench analyzes latency consistency, connection reuse, throughput stability, and queueing overhead to produce a concrete health verdict (GOOD, OK, WARN, BAD).

### Key Features

* Zero External Dependencies: Built purely with the Go standard library (`net/http`, `crypto/tls`, `sync`, `time`).
* Automated Performance Verdict: 8-point automated heuristics test covering reliability, Apdex satisfaction, tail latency spikes, and stability.
* Detailed Latency Percentiles: Low-overhead log-scale histogram providing min, mean, stddev, p50, p75, p90, p95, p99, p99.9, and max latency.
* Connection Timing Breakdown: Accurately separates DNS lookup, TCP handshake, TLS negotiation, Time to First Byte (TTFB), and body download duration.
* Rate Limiting (-q): Global token-bucket rate cap across all concurrent workers to prevent client-side queue distortion.
* CI/CD Quality Gates: Exit codes and `-strict` / `-fail-rate` flags for automated build pipeline assertions.
* Structured Output: Supports both ASCII human-readable terminal reports and machine-readable JSON (`-json`).

---

## Architecture and Timing Breakdown

gobench models the full HTTP lifecycle using `net/http/httptrace` to isolate latency bottlenecks:

```
[ Client Request ]
       |
       +---> DNS Resolution   (Skipped on connection reuse)
       |
       +---> TCP Handshake    (Skipped on connection reuse)
       |
       +---> TLS Negotiation  (Skipped on connection reuse)
       |
       +---> Request Dispatch
       |
       +---> TTFB (Waiting)   Server processing + network round-trip
       |
       +---> Download         Payload transfer duration
       |
[ Response Complete ]
```

---

## Installation and Build

### Prerequisites
* Go 1.21 or later installed.

### Build Binary
Clone the repository and build using standard Go tooling:

```bash
# Clone repository
git clone https://github.com/JRoetscyber/Gobench.git
cd Gobench

# Build single binary
go build -ldflags="-s -w" -o gobench.exe .     # Windows
go build -ldflags="-s -w" -o gobench .         # Linux / macOS
```

### Run Unit Tests
```bash
go test -v ./...
```

---

## Quick Start and Usage Examples

### 1. Basic Benchmark
Send 1,000 requests using 10 concurrent workers:
```bash
gobench -u https://example.com/ -n 1000 -c 10
```

### 2. Time-Bounded Load Test with Rate Limiting
Run for 30 seconds with 50 workers, capped at 200 requests per second:
```bash
gobench -d 30s -c 50 -q 200 https://api.example.com/v1/health
```

### 3. POST JSON Payload
Benchmark an API endpoint sending JSON from a file:
```bash
gobench -m POST -H "Content-Type: application/json" -body-file payload.json -n 500 -c 20 -u https://api.example.com/orders
```

### 4. CI/CD Automated Quality Gate
Enforce strict performance thresholds in CI (fails with exit code 2 on BAD verdict or > 0.5% errors):
```bash
gobench -strict -fail-rate 0.5 -apdex-t 300ms -n 2000 -c 25 -u https://staging.example.com/
```

### 5. Inspect Metric Explanations
Print the built-in performance glossary explaining Little's Law, tail latency, TTFB, and Apdex:
```bash
gobench -explain
```

---

## Sample Terminal Output

```
================================================================================
GOBENCH REPORT
Target:           https://api.example.com/v1/health
Method:           GET
Concurrency:      20 workers
Duration:         10.02s
Requests:         2,000 completed (100.0% success, 0 failed)
Throughput:       199.58 req/s (0.84 MB/s)
Connection Reuse: 99.0% (20 new connections, 1,980 reused)

--- Latency Summary (ms) ---
min:     12.4ms   p50:    24.1ms   p90:    38.5ms   p99:    64.2ms
mean:    26.8ms   p75:    29.6ms   p95:    44.0ms   p99.9:  92.1ms
stddev:  11.2ms   max:   104.5ms

--- Phase Breakdown (mean) ---
DNS Lookup:       1.2ms (new conns only)
TCP Handshake:    8.4ms (new conns only)
TLS Negotiation: 14.1ms (new conns only)
Server TTFB:     22.3ms
Body Download:    2.1ms

--- Performance Verdict: GOOD ---
[CHECK] Reliability    0.0% failures              -> GOOD  (Zero error tolerance met)
[CHECK] Speed          p95 44.0ms <= 500.0ms      -> GOOD  (Well within Apdex target)
[CHECK] Consistency    p99/p50 ratio 2.66 <= 3.00 -> GOOD  (Predictable tail latency)
[CHECK] Queueing       p50/min ratio 1.94 <= 3.00 -> OK    (Minimal server backlog)
[CHECK] Stability      98.2% late/early rate      -> GOOD  (Throughput sustained)
[CHECK] Connections    1.0% new conn ratio        -> GOOD  (Keep-alive optimized)
[CHECK] Satisfaction   Apdex 0.98 >= 0.94         -> GOOD  (High user satisfaction)
================================================================================
```

---

## Automated Verdict Heuristics

The built-in evaluation engine analyzes each run against industry performance standards. The overall run score reflects the lowest check grade:

| Metric Check | Description | GOOD | OK | WARN | BAD |
| :--- | :--- | :--- | :--- | :--- | :--- |
| Reliability | Failure rate (HTTP 4xx/5xx + network errors) | 0.0% | <= 0.1% | <= 1.0% | > 1.0% |
| Speed | 95th percentile latency vs Apdex target (T) | <= T | <= 2T | <= 4T | > 4T |
| Consistency | Tail ratio (`p99 / p50`) measuring jitter | <= 3.0 | <= 6.0 | <= 10.0 | > 10.0 |
| Queueing | Backlog ratio (`p50 / min`) under load | <= 1.5 | <= 3.0 | <= 6.0 | > 6.0 |
| Stability | Second-half throughput vs first-half throughput | >= 90% | >= 75% | >= 50% | < 50% |
| Connections | Percentage of requests needing new handshake | <= 2.0% | <= 10.0% | <= 30.0% | > 30.0% |
| Satisfaction | Apdex index (`(satisfied + tolerating/2) / total`) | >= 0.94 | >= 0.85 | >= 0.70 | < 0.70 |
| Target RPS | Achieved throughput vs `-target-rps` (if set) | >= 100% | >= 80% | >= 50% | < 50% |

---

## Command-Line Options

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-u` | string | `""` | Target URL (http:// or https://) |
| `-n` | int64 | `1000` | Total number of requests to execute |
| `-d` | duration | `0s` | Maximum benchmark duration (e.g. `30s`, `2m`) |
| `-c` | int | `10` | Concurrency: number of parallel worker goroutines |
| `-q` | float64 | `0` | Rate limiter: maximum total requests per second |
| `-m` | string | `"GET"` | HTTP method (`GET`, `POST`, `PUT`, `DELETE`, etc.) |
| `-H` | string | `""` | Additional header `Name: value` (repeatable) |
| `-body` | string | `""` | Raw string payload for request body |
| `-body-file` | string | `""` | Path to file containing request payload |
| `-timeout` | duration | `30s` | Per-request timeout limit |
| `-keepalive` | bool | `true` | Enable HTTP persistent connections (keep-alive) |
| `-insecure` | bool | `false` | Skip TLS certificate verification |
| `-no-redirect` | bool | `false` | Disable following 3xx HTTP redirects |
| `-json` | bool | `false` | Output complete report as structured JSON |
| `-progress` | bool | `false` | Print progress updates to stderr every 5 seconds |
| `-fail-rate` | float64 | `-1` | Threshold failure percentage triggering exit code 2 |
| `-apdex-t` | duration | `500ms` | Target response time for Apdex score and speed check |
| `-target-rps` | float64 | `0` | Required requests/sec to evaluate against |
| `-strict` | bool | `false` | Fail with exit code 2 if the verdict is BAD |
| `-explain` | bool | `false` | Include detailed metric glossary in output |
| `-version` | bool | `false` | Print version information and exit |

---

## Exit Codes for CI/CD

gobench is built to integrate directly into continuous deployment pipelines:

* `0`: Success. Benchmark completed and all criteria passed.
* `1`: Configuration error, invalid flag, unresolvable target, or runtime panic.
* `2`: Quality gate violation. Triggered when failure rate exceeds `-fail-rate` or when `-strict` is enabled and verdict is BAD.

### GitHub Actions Example

```yaml
name: Performance Gate
on: [push, pull_request]

jobs:
  load-test:
    runs-on: ubuntu-latest
    steps:
      - name: Check out repository
        uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.21'

      - name: Build gobench
        run: go build -o gobench .

      - name: Run Performance Check
        run: |
          ./gobench \
            -u https://staging.internal.net/health \
            -n 2000 \
            -c 20 \
            -apdex-t 250ms \
            -strict \
            -fail-rate 1.0
```

---

## Safety and Responsible Testing

Load testing can degrade production availability and incur significant cloud infrastructure costs.
* Only benchmark servers and domains you own or have explicit authorization to test.
* Avoid benchmarking public endpoints, CDNs, or shared third-party APIs without prior coordination.
