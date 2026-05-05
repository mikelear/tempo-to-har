// Package store handles GCS uploads of synthesized HAR files.
//
// Spike scope: simplest possible — invoke `gsutil cp` from $PATH.
// Avoids adding the cloud.google.com/go/storage SDK dep for a single
// upload operation. Phase 1 hardening: switch to native SDK + retries.
package store

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
)

// Upload writes content to gs://<bucket>/<path>.
// Caller is responsible for ensuring `gsutil` (or `gcloud storage`) is
// on PATH and authenticated.
func Upload(bucket, path string, content []byte) error {
	gsURL := fmt.Sprintf("gs://%s/%s", bucket, path)
	// #nosec G204 — bucket+path come from operator-controlled env/config, not user input.
	// Phase 1 hardening replaces this shellout with cloud.google.com/go/storage.
	cmd := exec.Command("gsutil", "cp", "-", gsURL)
	cmd.Stdin = bytes.NewReader(content)

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gsutil cp %s: %w (%s)", gsURL, err, stderr.String())
	}
	return nil
}
