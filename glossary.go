package main

const glossary = `HOW TO READ THIS REPORT

Load
  Concurrency (-c)   How many requests are in flight at once, like people queueing at a counter.
  Throughput         Completed requests per second. Rule of thumb (Little's law):
                     throughput = concurrency / mean latency. With too few workers you measure
                     your own patience, not the server. Raise -c until throughput stops growing;
                     that plateau is the capacity.
  Saturation         Past the capacity, more concurrency no longer adds throughput. It only adds
                     latency (queueing) and then errors.

Latency (request sent -> full response received)
  min                The best case: about what a lone request costs (network distance + minimum work).
  mean               The average. Easily dragged up by a few very slow requests.
  p50 (median)       Half of the requests were faster than this. The "typical" experience.
  p90/p95/p99/p99.9  The time that 90/95/99/99.9% of requests beat. p95 = 1 in 20 was slower.
                     Percentiles matter because people feel the slow ones: a page that makes 50
                     requests hits a p99-slow one about 4 times in 10.
  stddev             The spread. Large compared with the mean means inconsistent response times.
  max                The single worst request. One-off outliers are normal, so do not obsess over it.
  Distribution       A histogram on a log scale. One tall bar = consistent. A long tail to the right =
                     slow outliers. Two humps = two behaviours (cached vs uncached, retries, ...).

Where the time goes
  Waiting (TTFB)     Time from finishing sending the request to the first byte of the reply: server
                     processing plus one network round trip plus any queueing. High = slow backend.
  Download           Time to receive the rest of the reply. Grows with response size and slow links.
  DNS / TCP / TLS    Costs of opening a NEW connection: name lookup, TCP handshake, encryption setup.
                     Reused (keep-alive) connections skip all three.
  Connections        New vs reused. Ideally about one new connection per worker, then reuse.

Errors
  HTTP 2xx = OK, 3xx = redirect, 4xx = client error (429 rate limited, 403 blocked, 404 not found),
  5xx = server error (500 bug/crash, 502/503/504 overloaded or unreachable backend).
  timeout            No answer within -timeout.
  connection         Refused or reset: the server, a firewall or a connection limit.
  dns / tls          Name lookup failed / certificate or handshake problem.

Verdict checks
  Reliability        Percent of failed requests (errors and HTTP 400+).
  Speed              p95 latency against your target (-apdex-t, default 500 ms).
  Consistency        p99 / p50. Are there unpredictable slow spikes?
  Queueing           p50 / min. Near 1 = served immediately. Much higher = waiting in line.
  Stability          Throughput late in the run vs early. A drop means the server degrades over time.
  Connections        Is keep-alive working, or is every request paying for a new handshake?
  Satisfaction       Apdex score 0-1 (0.94+ excellent, 0.85+ good, 0.70+ fair, below that poor).
  Levels: GOOD, OK (minor), WARN (noticeable), BAD (fix this). The overall verdict is the worst check.

Caveats
  - This tool waits for each response before sending the next one (closed loop). When the server
    stalls it simply sends less, so slow-request numbers under overload look better than what real
    users would see. Use -q to hold a fixed request rate instead.
  - Your own PC, Wi-Fi, ISP, or a CDN/WAF in front of the site can be the bottleneck or can block you.
    Repeat the test, and compare a low -c run (baseline) with higher ones.
  - min latency includes network distance to the server; it cannot be lower than the ping time.
  - Percentiles come from a log-scale histogram with about 2% precision.
`
