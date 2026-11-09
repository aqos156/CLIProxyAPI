package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

type hermesHubEntry struct {
	body      string
	expiresAt time.Time
}

type hermesHub struct {
	secret  [32]byte
	client  *http.Client
	refresh singleflight.Group
	mu      sync.Mutex
	quotas  map[string]hermesHubEntry
}

func (s *Server) registerHermesHub() {
	hub := &hermesHub{client: auth.NewSubscriptionQuotaClient(), quotas: make(map[string]hermesHubEntry)}
	rand.Read(hub.secret[:])
	routes := s.engine.Group("/hermes-hub", AuthMiddleware(s.accessManager), func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if c.GetString("userApiKey") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "device API key required"})
		}
	})
	routes.GET("/auth-files", func(c *gin.Context) {
		files := make([]gin.H, 0)
		for _, credential := range s.hermesHubCredentials() {
			id := hub.accountID(credential.ID)
			files = append(files, gin.H{"id": id, "auth_index": id, "provider": credential.Provider})
		}
		sort.Slice(files, func(i, j int) bool { return files[i]["id"].(string) < files[j]["id"].(string) })
		c.JSON(http.StatusOK, gin.H{"files": files})
	})
	routes.POST("/api-call", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var request struct {
			Index    string `json:"auth_index"`
			Method   string `json:"method"`
			URL      string `json:"url"`
			Data     string `json:"data"`
			ProxyURL string `json:"proxy_url"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota request"})
			return
		}
		if request.Method != http.MethodGet || request.Data != "" || request.ProxyURL != "" ||
			(request.URL != auth.ClaudeSubscriptionQuotaURL && request.URL != auth.CodexSubscriptionQuotaURL) {
			c.JSON(http.StatusForbidden, gin.H{"error": "only subscription quota reads are allowed"})
			return
		}
		for _, credential := range s.hermesHubCredentials() {
			if hub.accountID(credential.ID) == request.Index && auth.SubscriptionQuotaURL(credential) == request.URL {
				body := hub.quota(c.Request.Context(), credential)
				if body == "" {
					c.JSON(http.StatusBadGateway, gin.H{"error": "provider quota unavailable"})
					return
				}
				c.JSON(http.StatusOK, gin.H{"status_code": http.StatusOK, "body": body})
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "account or quota endpoint unavailable"})
	})
}

func (s *Server) hermesHubCredentials() []*auth.Auth {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled || strings.TrimSpace(cfg.ProxyURL) != "" || s.handlers.AuthManager == nil {
		return nil
	}
	credentials := s.handlers.AuthManager.List()
	eligible := credentials[:0]
	for _, credential := range credentials {
		if auth.SubscriptionQuotaURL(credential) != "" {
			eligible = append(eligible, credential)
		}
	}
	return eligible
}

func (h *hermesHub) accountID(id string) string {
	digest := hmac.New(sha256.New, h.secret[:])
	_, _ = digest.Write([]byte(id))
	return hex.EncodeToString(digest.Sum(nil)[:16])
}

func (h *hermesHub) quota(ctx context.Context, credential *auth.Auth) string {
	key := fmt.Sprintf("%s:%d:%d", h.accountID(credential.ID), credential.RegistrationEpoch, credential.CredentialVersion)
	cached := func() (hermesHubEntry, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		entry, ok := h.quotas[key]
		return entry, ok && time.Now().Before(entry.expiresAt)
	}
	if entry, ok := cached(); ok {
		return entry.body
	}
	result := h.refresh.DoChan(key, func() (any, error) {
		if entry, ok := cached(); ok {
			return entry.body, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		document, err := auth.ReadSubscriptionQuota(fetchCtx, h.client, credential)
		entry := hermesHubEntry{expiresAt: time.Now().Add(30 * time.Second)}
		if err == nil {
			entry.body = projectHermesQuota(document, credential.Provider)
			entry.expiresAt = time.Now().Add(time.Minute)
		}
		h.mu.Lock()
		for id, previous := range h.quotas {
			if time.Now().After(previous.expiresAt) {
				delete(h.quotas, id)
			}
		}
		h.quotas[key] = entry
		h.mu.Unlock()
		return entry.body, nil
	})
	select {
	case <-ctx.Done():
		return ""
	case value := <-result:
		body, _ := value.Val.(string)
		return body
	}
}

func projectHermesQuota(root ojson.Result, provider string) string {
	doc := ojson.NewDocString("{}")
	if provider == "claude" {
		for _, name := range []string{"five_hour", "seven_day"} {
			if fields := hermesQuotaWindow(root.Get(name), "utilization", "resets_at", false); fields != nil {
				doc.Set(name, fields)
			}
		}
	} else {
		windows := make(map[string]any)
		for _, name := range []string{"primary_window", "secondary_window"} {
			window := root.Get("rate_limit").Get(name)
			if fields := hermesQuotaWindow(window, "used_percent", "reset_at", true); fields != nil {
				windows[name] = fields
			}
		}
		if len(windows) > 0 {
			doc.Set("rate_limit", windows)
		}
	}
	if doc.String() == "{}" {
		return ""
	}
	return doc.String()
}

func hermesQuotaWindow(window ojson.Result, percentKey, resetKey string, unixReset bool) map[string]any {
	percent := window.Get(percentKey)
	if percent.Type != ojson.Number || percent.Float() < 0 || percent.Float() > 100 {
		return nil
	}
	fields := map[string]any{percentKey: percent.Float(), resetKey: nil}
	reset := window.Get(resetKey)
	if unixReset {
		if reset.Type == ojson.Number && reset.Int() > 0 {
			fields[resetKey] = reset.Int()
		}
		if seconds := window.Get("limit_window_seconds"); seconds.Type == ojson.Number && seconds.Int() > 0 {
			fields["limit_window_seconds"] = seconds.Int()
		}
	} else if timestamp, err := time.Parse(time.RFC3339Nano, reset.String()); err == nil {
		fields[resetKey] = timestamp.UTC().Format(time.RFC3339)
	}
	return fields
}
