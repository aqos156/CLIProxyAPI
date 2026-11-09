package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

const ClaudeSubscriptionQuotaURL = "https://api.anthropic.com/api/oauth/usage"
const CodexSubscriptionQuotaURL = "https://chatgpt.com/backend-api/wham/usage"

// NewSubscriptionQuotaClient creates a transport that sends OAuth credentials only to
// the configured provider endpoint. Redirects and implicit environment proxies are disabled.
func NewSubscriptionQuotaClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 4
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// SubscriptionQuotaURL returns the fixed endpoint for a supported OAuth credential.
func SubscriptionQuotaURL(auth *Auth) string {
	credential, ok := subscriptionQuotaCredential(auth)
	if !ok {
		return ""
	}
	if credential.provider == "claude" {
		return ClaudeSubscriptionQuotaURL
	}
	return CodexSubscriptionQuotaURL
}

func subscriptionQuotaCredential(auth *Auth) (quotaCredential, bool) {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return quotaCredential{}, false
	}
	provider := strings.ToLower(auth.Provider)
	token, _ := auth.Metadata["access_token"].(string)
	accountID, _ := auth.Metadata["account_id"].(string)
	baseURL, _ := auth.Metadata["base_url"].(string)
	if (provider != "claude" && provider != "codex") || auth.AuthKind() != AuthKindOAuth ||
		token == "" || auth.ProxyURL != "" || auth.Attributes["base_url"] != "" || baseURL != "" || IsCodexUseSIWC(auth) {
		return quotaCredential{}, false
	}
	return quotaCredential{
		id: auth.ID, provider: provider, accessToken: token, accountID: accountID,
		epoch: auth.RegistrationEpoch, version: auth.CredentialVersion,
	}, true
}

// ReadSubscriptionQuota is a fixed, read-only provider operation. Consumers must
// project the returned document to the fields they are authorized to disclose.
func ReadSubscriptionQuota(ctx context.Context, client *http.Client, auth *Auth) (ojson.Result, error) {
	credential, ok := subscriptionQuotaCredential(auth)
	if !ok {
		return ojson.Result{}, errors.New("subscription quota unavailable")
	}
	return fetchSubscriptionQuota(ctx, client, credential)
}

func fetchSubscriptionQuota(ctx context.Context, client *http.Client, credential quotaCredential) (ojson.Result, error) {
	endpoint := ClaudeSubscriptionQuotaURL
	if credential.provider == "codex" {
		endpoint = CodexSubscriptionQuotaURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ojson.Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.accessToken)
	req.Header.Set("Accept", "application/json")
	if credential.provider == "claude" {
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	} else if credential.accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", credential.accountID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ojson.Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ojson.Result{}, errors.New("quota endpoint rejected request")
	}
	const maxBytes = 128 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || len(body) > maxBytes || !ojson.ValidBytes(body) {
		return ojson.Result{}, errors.New("invalid quota response")
	}
	return ojson.ParseBytes(body), nil
}
