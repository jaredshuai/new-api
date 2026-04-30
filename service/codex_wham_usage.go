package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

type CodexWhamRateLimitWindow struct {
	UsedPercent        *float64 `json:"used_percent,omitempty"`
	ResetAt            *int64   `json:"reset_at,omitempty"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds,omitempty"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds,omitempty"`
}

type CodexWhamRateLimit struct {
	PlanType        string                    `json:"plan_type,omitempty"`
	Allowed         *bool                     `json:"allowed,omitempty"`
	LimitReached    *bool                     `json:"limit_reached,omitempty"`
	PrimaryWindow   *CodexWhamRateLimitWindow `json:"primary_window,omitempty"`
	SecondaryWindow *CodexWhamRateLimitWindow `json:"secondary_window,omitempty"`
}

type CodexWhamUsagePayload struct {
	PlanType  string              `json:"plan_type,omitempty"`
	RateLimit *CodexWhamRateLimit `json:"rate_limit,omitempty"`
}

type codexUsageQuotaWindows struct {
	fiveHour *CodexWhamRateLimitWindow
	weekly   *CodexWhamRateLimitWindow
}

func FetchCodexWhamUsage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	if client == nil {
		return 0, nil, fmt.Errorf("nil http client")
	}
	bu := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if bu == "" {
		return 0, nil, fmt.Errorf("empty baseURL")
	}
	at := strings.TrimSpace(accessToken)
	aid := strings.TrimSpace(accountID)
	if at == "" {
		return 0, nil, fmt.Errorf("empty accessToken")
	}
	if aid == "" {
		return 0, nil, fmt.Errorf("empty accountID")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bu+"/backend-api/wham/usage", nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("chatgpt-account-id", aid)
	req.Header.Set("Accept", "application/json")
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "codex_cli_rs")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func ParseCodexWhamUsage(body []byte) (*CodexWhamUsagePayload, error) {
	var payload CodexWhamUsagePayload
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func CodexWhamUsageAutoEnableTime(body []byte, now time.Time) (time.Time, string, bool, error) {
	payload, err := ParseCodexWhamUsage(body)
	if err != nil {
		return time.Time{}, "", false, err
	}
	if payload == nil || payload.RateLimit == nil {
		return time.Time{}, "missing codex rate_limit data", false, nil
	}

	windows := resolveCodexUsageQuotaWindows(payload)
	resetAt := time.Time{}
	reasons := make([]string, 0, 2)
	missingResetReasons := make([]string, 0, 2)
	addReset := func(label string, window *CodexWhamRateLimitWindow) {
		if !codexQuotaWindowExhausted(window) {
			return
		}
		windowResetAt, ok := codexQuotaWindowResetTime(window, now)
		if !ok {
			missingResetReasons = append(missingResetReasons, label)
			return
		}
		if resetAt.IsZero() || windowResetAt.After(resetAt) {
			resetAt = windowResetAt
		}
		reasons = append(reasons, label)
	}

	addReset("5-hour", windows.fiveHour)
	addReset("weekly", windows.weekly)

	if len(missingResetReasons) > 0 {
		return time.Time{}, fmt.Sprintf("codex exhausted %s quota window missing reset time", strings.Join(missingResetReasons, " and ")), false, nil
	}
	if resetAt.IsZero() {
		return time.Time{}, "codex usage does not expose an exhausted quota reset time", false, nil
	}
	if resetAt.Before(now) {
		resetAt = now
	}
	return resetAt, fmt.Sprintf("codex %s quota window reset", strings.Join(reasons, " and ")), true, nil
}

func CodexWhamUsageQuotaAvailable(body []byte) (bool, string, error) {
	payload, err := ParseCodexWhamUsage(body)
	if err != nil {
		return false, "", err
	}
	if payload == nil || payload.RateLimit == nil {
		return false, "missing codex rate_limit data", nil
	}

	rateLimit := payload.RateLimit
	if rateLimit.Allowed != nil && *rateLimit.Allowed {
		if rateLimit.LimitReached == nil || !*rateLimit.LimitReached {
			return true, "codex usage is currently allowed", nil
		}
	}
	return false, "codex usage is not currently allowed", nil
}

func resolveCodexUsageQuotaWindows(payload *CodexWhamUsagePayload) codexUsageQuotaWindows {
	if payload == nil || payload.RateLimit == nil {
		return codexUsageQuotaWindows{}
	}

	rateLimit := payload.RateLimit
	planType := strings.ToLower(strings.TrimSpace(payload.PlanType))
	if planType == "" {
		planType = strings.ToLower(strings.TrimSpace(rateLimit.PlanType))
	}

	windows := []*CodexWhamRateLimitWindow{rateLimit.PrimaryWindow, rateLimit.SecondaryWindow}
	result := codexUsageQuotaWindows{}
	for _, window := range windows {
		switch classifyCodexQuotaWindow(window) {
		case "five_hour":
			if result.fiveHour == nil {
				result.fiveHour = window
			}
		case "weekly":
			if result.weekly == nil {
				result.weekly = window
			}
		}
	}

	if planType == "free" {
		if result.weekly == nil {
			result.weekly = firstNonNilCodexQuotaWindow(windows)
		}
		return result
	}

	if result.fiveHour == nil && result.weekly == nil {
		result.fiveHour = rateLimit.PrimaryWindow
		result.weekly = rateLimit.SecondaryWindow
		return result
	}
	if result.fiveHour == nil {
		result.fiveHour = firstCodexQuotaWindowExcept(windows, result.weekly)
	}
	if result.weekly == nil {
		result.weekly = firstCodexQuotaWindowExcept(windows, result.fiveHour)
	}
	return result
}

func classifyCodexQuotaWindow(window *CodexWhamRateLimitWindow) string {
	if window == nil || window.LimitWindowSeconds == nil || *window.LimitWindowSeconds <= 0 {
		return ""
	}
	if *window.LimitWindowSeconds >= 24*60*60 {
		return "weekly"
	}
	return "five_hour"
}

func codexQuotaWindowExhausted(window *CodexWhamRateLimitWindow) bool {
	if window == nil || window.UsedPercent == nil {
		return false
	}
	return *window.UsedPercent >= 100
}

func codexQuotaWindowResetTime(window *CodexWhamRateLimitWindow, now time.Time) (time.Time, bool) {
	if window == nil {
		return time.Time{}, false
	}
	if window.ResetAfterSeconds != nil && *window.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(*window.ResetAfterSeconds) * time.Second), true
	}
	if window.ResetAt != nil && *window.ResetAt > 0 {
		return time.Unix(*window.ResetAt, 0), true
	}
	return time.Time{}, false
}

func firstNonNilCodexQuotaWindow(windows []*CodexWhamRateLimitWindow) *CodexWhamRateLimitWindow {
	for _, window := range windows {
		if window != nil {
			return window
		}
	}
	return nil
}

func firstCodexQuotaWindowExcept(windows []*CodexWhamRateLimitWindow, excluded *CodexWhamRateLimitWindow) *CodexWhamRateLimitWindow {
	for _, window := range windows {
		if window != nil && window != excluded {
			return window
		}
	}
	return nil
}
