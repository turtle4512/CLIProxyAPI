package helps

import (
	"testing"
	"time"
)

// #2170 — upstream-declared Google retry/quota-reset delays are capped at 6h
// (same source of truth as claudeMaxRateLimitCooldown) so a daily-quota
// header cannot strand the credential for ~24h via the antigravity tokens
// path (sErr.retryAfter -> conductor MarkResult).
func TestParseRetryDelay_CappedAt6h(t *testing.T) {
	t.Run("retryDelay beyond cap is clamped", func(t *testing.T) {
		body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"172h"}]}}`)
		got, err := ParseRetryDelay(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if *got != maxGoogleRetryDelay {
			t.Fatalf("expected %v (capped), got %v", maxGoogleRetryDelay, *got)
		}
	})

	t.Run("quotaResetDelay beyond cap is clamped", func(t *testing.T) {
		body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetDelay":"25h"}}]}}`)
		got, err := ParseRetryDelay(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if *got != maxGoogleRetryDelay {
			t.Fatalf("expected %v (capped), got %v", maxGoogleRetryDelay, *got)
		}
	})

	t.Run("message 'after Ns' beyond cap is clamped", func(t *testing.T) {
		body := []byte(`{"error":{"message":"Rate limit exceeded. Retry after 86400s."}}`)
		got, err := ParseRetryDelay(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if *got != maxGoogleRetryDelay {
			t.Fatalf("expected %v (capped), got %v", maxGoogleRetryDelay, *got)
		}
	})

	t.Run("short delay is preserved, never extended (#2170 AC2)", func(t *testing.T) {
		body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`)
		got, err := ParseRetryDelay(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if *got != 30*time.Second {
			t.Fatalf("expected 30s preserved, got %v", *got)
		}
	})

	t.Run("no retry info still errors", func(t *testing.T) {
		got, err := ParseRetryDelay([]byte(`{"error":{"message":"unrelated"}}`))
		if err == nil || got != nil {
			t.Fatalf("expected error, got %v / %v", got, err)
		}
	})
}
