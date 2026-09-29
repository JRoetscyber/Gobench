// gobench is a small HTTP load-testing tool in the spirit of ApacheBench,
// with a built-in verdict that explains whether the results are good or bad.
// Only test servers you own or have permission to test.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Exit codes: 0 ok, 1 usage/runtime error, 2 failure rate above -fail-rate or (-strict) BAD verdict.
func realMain(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "gobench:", err)
		return 1
	case cfg.Version:
		fmt.Fprintln(stdout, "gobench", version)
		return 0
	case cfg.GlossaryOnly:
		fmt.Fprint(stdout, glossary)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second Ctrl+C force-quits

	client := newClient(cfg)
	prog := &progress{}
	if cfg.Progress {
		pctx, pcancel := context.WithCancel(ctx)
		defer pcancel()
		go reportProgress(pctx, stderr, prog, time.Now(), 5*time.Second)
	}

	res := run(ctx, cfg, client, prog)
	client.CloseIdleConnections()

	rep := buildReport(cfg, res)
	if rep.Total == 0 {
		fmt.Fprintln(stderr, "gobench: no requests completed")
		return 1
	}
	if cfg.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "gobench:", err)
			return 1
		}
	} else {
		printText(stdout, rep, cfg.Explain)
		if cfg.Explain {
			fmt.Fprintln(stdout)
			fmt.Fprint(stdout, glossary)
		}
	}

	if cfg.FailRate >= 0 && rep.FailedPercent > cfg.FailRate {
		fmt.Fprintf(stderr, "gobench: failure rate %.2f%% exceeds -fail-rate %.2f%%\n", rep.FailedPercent, cfg.FailRate)
		return 2
	}
	if cfg.Strict && rep.Verdict.Overall == levelBad {
		fmt.Fprintln(stderr, "gobench: verdict is BAD (-strict)")
		return 2
	}
	return 0
}

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }
