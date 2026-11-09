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

func TestSoonestResetPreservesCodexWebSocketPreference(t *testing.T) {
	now := time.Now()
	httpOnly := &Auth{ID: "http", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic"}}
	websocket := &Auth{ID: "websocket", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic", "websockets": true}}
	selector := &SoonestResetSelector{now: func() time.Time { return now }, cache: map[string]quotaEntry{
		"http":      {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now, weekly: quotaWindow{reset: now.Add(time.Hour)}}},
		"websocket": {next: now.Add(time.Minute), snapshot: quotaSnapshot{observed: now, weekly: quotaWindow{reset: now.Add(24 * time.Hour)}}},
	}}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, err := selector.Pick(ctx, "codex", "gpt-6.1-sol", cliproxyexecutor.Options{}, []*Auth{httpOnly, websocket})
	if err != nil || got != websocket {
		t.Fatalf("WebSocket selection: got %v, %v; want websocket credential", got, err)
	}
}

func TestSoonestResetProtectsFivePercentReserve(t *testing.T) {
	now := time.Now()
	early := &Auth{ID: "early", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-a"}}
	late := &Auth{ID: "late", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-b"}}
	for _, tc := range []struct {
		name                           string
		earlyUsed, lateUsed, shortUsed float64
		sameReset                      bool
		want                           *Auth
	}{
		{name: "one percent left versus ninety-four", earlyUsed: 99, lateUsed: 6, want: late},
		{name: "exactly five percent is reserve", earlyUsed: 95, lateUsed: 6, want: late},
		{name: "both healthy prefer earlier reset", earlyUsed: 94, lateUsed: 6, want: early},
		{name: "both in reserve prefer earlier reset", earlyUsed: 99, lateUsed: 96, want: early},
		{name: "five hour limit can put account in reserve", earlyUsed: 6, lateUsed: 6, shortUsed: 96, want: late},
		{name: "same reset prefers more remaining", earlyUsed: 40, lateUsed: 6, sameReset: true, want: late},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlyReset := now.Add(16 * time.Hour)
			lateReset := now.Add(6 * 24 * time.Hour)
			if tc.sameReset {
				lateReset = earlyReset
			}
			selector := &SoonestResetSelector{now: func() time.Time { return now }, cache: map[string]quotaEntry{
				early.ID: {next: now.Add(time.Minute), snapshot: quotaSnapshot{
					observed: now, weekly: quotaWindow{reset: earlyReset, used: tc.earlyUsed},
					short: quotaWindow{reset: now.Add(time.Hour), used: tc.shortUsed},
				}},
				late.ID: {next: now.Add(time.Minute), snapshot: quotaSnapshot{
					observed: now, weekly: quotaWindow{reset: lateReset, used: tc.lateUsed},
				}},
			}}
			got, err := selector.Pick(context.Background(), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{early, late})
			if err != nil || got != tc.want {
				t.Fatalf("selection: got %v, %v; want %s", got, err, tc.want.ID)
			}
		})
	}
}
