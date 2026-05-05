// Package main is the tempo-to-har synthesizer CLI.
//
// Designed to run as a CronJob: queries Tempo for spans of a configured
// set of services within a window (default: last hour), synthesizes one
// HAR per trace, uploads each to GCS at:
//
//   gs://<bucket>/har/v1/tempo-synth/<service>/<traceID>.har
//
// Spike scope: hardcoded service list (leartech-qa-canary), single bucket,
// fire-and-exit. Phase 1 hardening: ConfigMap-driven service list,
// per-service window/filter config, multi-cluster fan-out, native GCS SDK.
//
// Required env (CronJob supplies these via ConfigMap or --flag):
//
//   TEMPO_BASE_URL   default: http://tempo-query-frontend.jx-observability.svc.cluster.local:3200
//   GCS_BUCKET       default: test-artifacts-product-first
//   GCS_PREFIX       default: har/v1/tempo-synth
//   SERVICE_NAME     required: which service to synthesize HAR for
//   WINDOW_MINUTES   default: 60
//   CLUSTER_TAG      default: unknown
//   LIMIT            default: 50 (max traces per run)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"encoding/json"

	"github.com/mikelear/tempo-to-har/internal/store"
	"github.com/mikelear/tempo-to-har/internal/synth"
	"github.com/mikelear/tempo-to-har/internal/tempo"
)

func main() {
	var (
		tempoURL    = flag.String("tempo-url", envOr("TEMPO_BASE_URL", "http://tempo-query-frontend.jx-observability.svc.cluster.local:3200"), "Tempo base URL")
		bucket      = flag.String("bucket", envOr("GCS_BUCKET", "test-artifacts-product-first"), "GCS bucket")
		prefix      = flag.String("prefix", envOr("GCS_PREFIX", "har/v1/tempo-synth"), "GCS path prefix (no trailing slash)")
		serviceName = flag.String("service", envOr("SERVICE_NAME", ""), "service.name to query Tempo for (required)")
		windowMin   = flag.Int("window-minutes", envIntOr("WINDOW_MINUTES", 60), "minutes back from now to query")
		clusterTag  = flag.String("cluster", envOr("CLUSTER_TAG", "unknown"), "cluster tag")
		limit       = flag.Int("limit", envIntOr("LIMIT", 50), "max traces per run")
		dryRun      = flag.Bool("dry-run", false, "log decisions but don't upload to GCS")
	)
	flag.Parse()

	if *serviceName == "" {
		fmt.Fprintln(os.Stderr, "FATAL: --service or SERVICE_NAME required")
		os.Exit(2)
	}

	logf := func(level, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", level, fmt.Sprintf(format, args...))
	}

	logf("info", "tempo-to-har starting (service=%s, tempo=%s, bucket=%s, prefix=%s, dry-run=%t)",
		*serviceName, *tempoURL, *bucket, *prefix, *dryRun)

	until := time.Now()
	since := until.Add(-time.Duration(*windowMin) * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := tempo.New(*tempoURL)
	traces, err := c.Search(ctx, *serviceName, since, until, *limit)
	if err != nil {
		logf("fatal", "Tempo search: %v", err)
		os.Exit(1)
	}
	logf("info", "search returned %d traces in window [%s, %s]", len(traces), since.Format(time.RFC3339), until.Format(time.RFC3339))

	if len(traces) == 0 {
		logf("info", "no traces — nothing to synthesize")
		return
	}

	uploaded := 0
	skipped := 0
	failed := 0
	for _, t := range traces {
		body, err := c.Trace(ctx, t.TraceID)
		if err != nil {
			logf("warn", "fetch trace %s: %v", t.TraceID, err)
			failed++
			continue
		}

		har, err := synth.SynthFromTempo(body, *serviceName, *clusterTag, "tempo")
		if err != nil {
			logf("warn", "synth trace %s: %v", t.TraceID, err)
			failed++
			continue
		}

		if len(har.Log.Entries) == 0 {
			// Trace had no HTTP-flavored spans — skip silently.
			skipped++
			continue
		}

		harBytes, err := json.MarshalIndent(har, "", "  ")
		if err != nil {
			logf("warn", "marshal HAR for trace %s: %v", t.TraceID, err)
			failed++
			continue
		}

		gcsPath := fmt.Sprintf("%s/%s/%s.har", *prefix, *serviceName, t.TraceID)
		if *dryRun {
			logf("info", "dry-run: would upload to gs://%s/%s (%d bytes, %d entries)",
				*bucket, gcsPath, len(harBytes), len(har.Log.Entries))
			uploaded++
			continue
		}

		if err := store.Upload(*bucket, gcsPath, harBytes); err != nil {
			logf("warn", "upload trace %s: %v", t.TraceID, err)
			failed++
			continue
		}
		logf("info", "uploaded gs://%s/%s (%d entries)", *bucket, gcsPath, len(har.Log.Entries))
		uploaded++
	}

	logf("info", "summary: uploaded=%d skipped=%d failed=%d total-traces=%d", uploaded, skipped, failed, len(traces))

	if failed > 0 {
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
