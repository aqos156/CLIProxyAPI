package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetPreservesSessionAffinity(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-a"}}
	b := &Auth{ID: "b", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-b"}}
	s := &SoonestResetSelector{now: func() time.Time { return now }, cache: map[string]quotaEntry{
		"a": {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now, weekly: quotaWindow{reset: now.Add(48 * time.Hour)}}},
		"b": {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now, weekly: quotaWindow{reset: now.Add(24 * time.Hour)}}},
	}}
	affinity := NewSessionAffinitySelector(s)
	t.Cleanup(affinity.Stop)
	assertPick := func(session, want string) {
		t.Helper()
		opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {session}}}
		got, err := affinity.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("session %s: got %v, %v; want %s", session, got, err, want)
		}
	}
	assertPick("original-session", "b")
	entry := s.cache["b"]
	entry.snapshot.weekly.reset = now.Add(72 * time.Hour)
	s.cache["b"] = entry
	assertPick("original-session", "b")
	assertPick("new-session", "a")
	entry = s.cache["a"]
	entry.snapshot.short = quotaWindow{reset: now.Add(time.Hour), used: 100}
	s.cache["a"] = entry
	assertPick("third-session", "b")
}

func TestSoonestResetFallsBackOnStaleQuota(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-a"}}
	b := &Auth{ID: "b", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-b"}}
	s := &SoonestResetSelector{now: func() time.Time { return now }, cache: map[string]quotaEntry{
		"a": {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now.Add(-quotaSnapshotLifetime), weekly: quotaWindow{reset: now.Add(time.Hour)}}},
		"b": {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now, weekly: quotaWindow{reset: now.Add(24 * time.Hour)}}},
	}}
	for _, want := range []string{"a", "b"} {
		got, err := s.Pick(context.Background(), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("round-robin fallback: got %v, %v; want %s", got, err, want)
		}
	}
}

func TestParseSoonestResetCodexUsesWeeklyWindow(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	input := ojson.ParseBytes([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":2000003600},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":2000086400}}}`))
	got, err := parseQuotaSnapshot(input, "codex", now)
	if err != nil || !got.weekly.reset.Equal(now.Add(24*time.Hour)) || got.weekly.used != 30 || !got.short.reset.Equal(now.Add(time.Hour)) {
		t.Fatalf("unexpected snapshot: %+v, %v", got, err)
	}
}
