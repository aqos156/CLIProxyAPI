package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const quotaRefreshInterval = 5 * time.Minute
const quotaSnapshotLifetime = 10 * time.Minute

type quotaWindow struct {
	reset time.Time
	used  float64
}

type quotaSnapshot struct {
	observed time.Time
	weekly   quotaWindow
	short    quotaWindow
	models   map[string]quotaWindow
}

type quotaCredential struct {
	id, provider, accessToken, accountID string
	epoch, version                       uint64
}

type quotaEntry struct {
	epoch, version uint64
	next           time.Time
	snapshot       quotaSnapshot
}

type SoonestResetSelector struct {
	fallback RoundRobinSelector
	client   *http.Client
	now      func() time.Time
	cancel   context.CancelFunc
	requests chan quotaCredential
	mu       sync.Mutex
	cache    map[string]quotaEntry
}

func NewSoonestResetSelector() *SoonestResetSelector {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SoonestResetSelector{
		client: NewSubscriptionQuotaClient(),
		now:    time.Now, cancel: cancel, requests: make(chan quotaCredential, 32), cache: make(map[string]quotaEntry),
	}
	go s.refresh(ctx)
	return s
}

func (s *SoonestResetSelector) Stop() {
	if s.cancel != nil {
		s.cancel()
		s.client.CloseIdleConnections()
	}
}

func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}
	if selected := s.pickQuota(available, model, now); selected != nil {
		return selected, nil
	}
	return s.fallback.Pick(ctx, provider, model, opts, available)
}

func (s *SoonestResetSelector) pickQuota(available []*Auth, model string, now time.Time) *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *Auth
	var earliest time.Time
	var poolProvider string
	known := true
	model = strings.ToLower(model)
	for _, auth := range available {
		credential, ok := subscriptionQuotaCredential(auth)
		if !ok {
			known = false
			continue
		}
		provider := credential.provider
		if poolProvider == "" {
			poolProvider = provider
		} else if poolProvider != provider {
			known = false
		}
		entry := s.cache[auth.ID]
		if entry.epoch != auth.RegistrationEpoch || entry.version != auth.CredentialVersion {
			entry = quotaEntry{epoch: auth.RegistrationEpoch, version: auth.CredentialVersion}
			delete(s.cache, auth.ID)
		}
		if !entry.next.After(now) {
			// Keep the cache bounded when credentials are regularly removed or replaced.
			if len(s.cache) >= 1024 {
				for id, old := range s.cache {
					if now.Sub(old.next) >= quotaSnapshotLifetime {
						delete(s.cache, id)
					}
				}
			}
			if len(s.cache) < 1024 {
				select {
				case s.requests <- credential:
					entry.next = now.Add(quotaRefreshInterval)
					s.cache[auth.ID] = entry
				default:
				}
			}
		}
		snapshot := entry.snapshot
		if snapshot.observed.IsZero() || now.Before(snapshot.observed) || now.Sub(snapshot.observed) >= quotaSnapshotLifetime || !snapshot.weekly.reset.After(now) {
			known = false
			continue
		}
		if exhausted(snapshot.weekly, now) || exhausted(snapshot.short, now) ||
			(strings.Contains(model, "sonnet") && exhausted(snapshot.models["sonnet"], now)) ||
			(strings.Contains(model, "opus") && exhausted(snapshot.models["opus"], now)) ||
			(strings.Contains(model, "fable") && exhausted(snapshot.models["fable"], now)) {
			continue
		}
		if selected == nil || snapshot.weekly.reset.Before(earliest) || snapshot.weekly.reset.Equal(earliest) && auth.ID < selected.ID {
			selected, earliest = auth, snapshot.weekly.reset
		}
	}
	if !known {
		return nil
	}
	return selected
}

func exhausted(window quotaWindow, now time.Time) bool {
	return window.used >= 100 && (window.reset.IsZero() || window.reset.After(now))
}

func (s *SoonestResetSelector) refresh(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case credential := <-s.requests:
			poll, cancel := context.WithTimeout(ctx, 8*time.Second)
			snapshot, err := s.fetchQuota(poll, credential)
			cancel()
			s.mu.Lock()
			entry, ok := s.cache[credential.id]
			if ok && entry.epoch == credential.epoch && entry.version == credential.version {
				if err == nil {
					entry.snapshot = snapshot
					for _, window := range []quotaWindow{snapshot.weekly, snapshot.short} {
						if window.reset.After(snapshot.observed) && window.reset.Before(entry.next) {
							entry.next = window.reset
						}
					}
				} else {
					entry.snapshot = quotaSnapshot{}
				}
				s.cache[credential.id] = entry
			}
			s.mu.Unlock()
		}
	}
}

func (s *SoonestResetSelector) fetchQuota(ctx context.Context, credential quotaCredential) (quotaSnapshot, error) {
	root, err := fetchSubscriptionQuota(ctx, s.client, credential)
	if err != nil {
		return quotaSnapshot{}, err
	}
	return parseQuotaSnapshot(root, credential.provider, s.now())
}

func parseQuotaSnapshot(root ojson.Result, provider string, now time.Time) (quotaSnapshot, error) {
	snapshot := quotaSnapshot{observed: now}
	if provider == "claude" {
		snapshot.weekly = claudeQuotaWindow(root.Get("seven_day"))
		snapshot.short = claudeQuotaWindow(root.Get("five_hour"))
		snapshot.models = map[string]quotaWindow{
			"opus": claudeQuotaWindow(root.Get("seven_day_opus")), "sonnet": claudeQuotaWindow(root.Get("seven_day_sonnet")),
			"fable": claudeQuotaWindow(root.Get("seven_day_fable")),
		}
	} else {
		limits := root.Get("rate_limit")
		for _, name := range []string{"primary_window", "secondary_window"} {
			data := limits.Get(name)
			if data.Get("used_percent").Type != ojson.Number {
				continue
			}
			window := quotaWindow{used: data.Get("used_percent").Float()}
			if epoch := data.Get("reset_at").Int(); epoch > 0 {
				window.reset = time.Unix(epoch, 0)
			}
			switch data.Get("limit_window_seconds").Int() {
			case 18000:
				snapshot.short = window
			case 604800:
				snapshot.weekly = window
			}
		}
		if limits.Get("limit_reached").Bool() || limits.Get("allowed").Exists() && !limits.Get("allowed").Bool() {
			snapshot.short.used = 100
		}
	}
	if !snapshot.weekly.reset.After(now) || snapshot.weekly.used < 0 || snapshot.weekly.used > 100 {
		return quotaSnapshot{}, errors.New("weekly quota unavailable")
	}
	return snapshot, nil
}

func claudeQuotaWindow(data ojson.Result) quotaWindow {
	if data.Get("utilization").Type != ojson.Number {
		return quotaWindow{}
	}
	reset, _ := time.Parse(time.RFC3339Nano, data.Get("resets_at").String())
	return quotaWindow{reset: reset, used: data.Get("utilization").Float()}
}
