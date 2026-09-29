# gobench

A small, dependency-free HTTP load tester in Go (ApacheBench-style) with a built-in
verdict that tells you whether the results are good or bad, and why.
Only test servers you own or have permission to test.

## Build

    go build -o gobench.exe .          # Windows
    go build -o gobench .              # Linux / macOS
    go test ./...

## Usage

    .\gobench -u https://yoursite.com/ -n 1000 -c 50
    .\gobench -d 30s -c 100 -q 500 https://yoursite.com/api/health
    .\gobench -explain -u https://yoursite.com/ -n 1000 -c 50     # report + what every metric means
    .\gobench -explain                                            # just the glossary
    .\gobench -m POST -H "Content-Type: application/json" -body-file payload.json -n 500 -c 20 -u https://yoursite.com/api/login
    .\gobench -json -strict -n 2000 -c 50 -u https://staging.example.com/   # CI gate

Run `gobench -h` for all flags. Ctrl+C stops early and prints partial results.

## What the report shows

- Throughput, failures, HTTP status codes, error kinds (timeout / connection / dns / tls)
- Latency: min, mean, stddev, p50, p75, p90, p95, p99, p99.9, max, plus a distribution chart
- Where the time goes: waiting for the server (TTFB), download, and DNS / TCP / TLS for new connections
- Connection reuse (keep-alive) and throughput over time
- A verdict (GOOD / OK / WARN / BAD) built from these checks:

| Check | GOOD | OK | WARN | BAD |
|---|---|---|---|---|
| Reliability (failed %) | 0 | <= 0.1 | <= 1 | > 1 |
| Speed (p95 vs -apdex-t = T) | <= T | <= 2T | <= 4T | > 4T |
| Consistency (p99 / p50) | <= 3 | <= 6 | <= 10 | > 10 |
| Queueing (p50 / min) | <= 1.5 | <= 3 | <= 6 | > 6 |
| Stability (late / early throughput) | >= 90% | >= 75% | >= 50% | < 50% |
| Connections (extra new conns) | <= 2% | <= 10% | <= 30% | > 30% |
| Satisfaction (Apdex) | >= 0.94 | >= 0.85 | >= 0.70 | < 0.70 |
| Target rate (only with -target-rps) | >= 100% | >= 80% | >= 50% | < 50% |

The overall verdict is the worst check. These thresholds are rules of thumb, not laws:
set `-apdex-t` to what is realistic for the page you test.

## Notes

- Failure = transport error or HTTP status >= 400.
- Latency covers completed responses only; the body is fully read.
- `-d` stops issuing new requests at the deadline; in-flight ones finish and are counted.
- `-q` is a global rate cap, so achieved rate can be lower if `-c` is too small for the latency.
- The tool is closed-loop (each worker waits for its response). Under overload this understates
  tail latency; use `-q` to hold a fixed rate.
- Percentiles come from a log-scale histogram (~2% precision, constant memory).
- Output is plain ASCII so it renders correctly in any Windows console.
- Exit codes: 0 ok, 1 usage/runtime error, 2 failure rate above `-fail-rate` or (with `-strict`) a BAD verdict.
- Run from a different machine than the server for realistic numbers.
