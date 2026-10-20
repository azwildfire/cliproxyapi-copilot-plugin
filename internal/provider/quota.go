package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/redact"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
)

// Quota support talks the host's quota-provider JSON contract directly (the pinned v7.2.118
// SDK predates pluginapi.QuotaProvider), so the request and response shapes are defined here.
// They match pluginapi.QuotaFetchRequest/QuotaFetchResponse in CLIProxyAPI v8.

const (
	quotaCacheTTL       = 30 * time.Second
	copilotUserEndpoint = "/copilot_internal/user"
)

// QuotaFetchRequest is the quota.fetch RPC payload.
type QuotaFetchRequest struct {
	AuthIndex      string            `json:"auth_index"`
	AuthID         string            `json:"auth_id"`
	Provider       string            `json:"provider"`
	StorageJSON    []byte            `json:"storage_json,omitempty"`
	Metadata       map[string]any    `json:"metadata,omitempty"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

// QuotaDescribeResponse advertises the providers this quota provider serves.
type QuotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

// QuotaSubscription is the plan block of the normalized quota.
type QuotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

// QuotaBucket is one metered window.
type QuotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// QuotaGroup groups buckets under one heading.
type QuotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []QuotaBucket `json:"buckets,omitempty"`
}

// QuotaMetric is a bounded numeric summary value.
type QuotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// QuotaFetchResponse is the normalized quota consumed by the management UI.
type QuotaFetchResponse struct {
	Subscription       *QuotaSubscription `json:"subscription,omitempty"`
	Summary            []QuotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []QuotaGroup       `json:"groups,omitempty"`
}

// QuotaResetResponse answers quota.reset.
type QuotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// copilotUser is the subset of GET /copilot_internal/user the quota card needs.
type copilotUser struct {
	Login             string                   `json:"login"`
	CopilotPlan       string                   `json:"copilot_plan"`
	AccessTypeSKU     string                   `json:"access_type_sku"`
	QuotaResetDate    string                   `json:"quota_reset_date"`
	QuotaResetDateUTC string                   `json:"quota_reset_date_utc"`
	TokenBasedBilling bool                     `json:"token_based_billing"`
	QuotaSnapshots    map[string]quotaSnapshot `json:"quota_snapshots"`
}

type quotaSnapshot struct {
	QuotaID            string  `json:"quota_id"`
	Entitlement        float64 `json:"entitlement"`
	Remaining          float64 `json:"remaining"`
	QuotaRemaining     float64 `json:"quota_remaining"`
	PercentRemaining   float64 `json:"percent_remaining"`
	Unlimited          bool    `json:"unlimited"`
	HasQuota           *bool   `json:"has_quota"`
	OverageCount       float64 `json:"overage_count"`
	OveragePermitted   bool    `json:"overage_permitted"`
	OverageEntitlement float64 `json:"overage_entitlement"`
	CreditsUsed        float64 `json:"credits_used"`
	TokenBasedBilling  bool    `json:"token_based_billing"`
	QuotaResetAt       float64 `json:"quota_reset_at"`
	TimestampUTC       string  `json:"timestamp_utc"`
}

type quotaCacheEntry struct {
	fingerprint string
	expiresAt   time.Time
	response    QuotaFetchResponse
}

// DescribeQuota registers this plugin as the quota source for copilot credentials.
func (s *Service) DescribeQuota() QuotaDescribeResponse {
	return QuotaDescribeResponse{
		SupportedProviders: []string{providerID},
		DisplayName:        "GitHub Copilot",
		SupportsReset:      false,
	}
}

// FetchQuota reads the Copilot plan and quota snapshots for a credential.
func (s *Service) FetchQuota(ctx context.Context, req QuotaFetchRequest) (QuotaFetchResponse, error) {
	if len(req.StorageJSON) == 0 {
		return QuotaFetchResponse{}, statusError("invalid_request", "credential storage is not available for quota lookup", http.StatusBadRequest)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return QuotaFetchResponse{}, statusError("invalid_credential", errParse.Error(), http.StatusUnauthorized)
	}
	fingerprint := tokenFingerprint(storage.GitHubAccessToken)
	cacheKey := strings.TrimSpace(req.AuthID)
	if cacheKey == "" {
		cacheKey = fingerprint
	}
	now := s.now()
	s.quotaMu.Lock()
	cached, ok := s.quotaEntries[cacheKey]
	s.quotaMu.Unlock()
	if ok && cached.fingerprint == fingerprint && cached.expiresAt.After(now) {
		return cached.response, nil
	}

	user, serverOffsetMs, errUser := s.fetchCopilotUser(ctx, req.HostCallbackID, storage.GitHubAccessToken)
	if errUser != nil {
		return QuotaFetchResponse{}, errUser
	}
	response := buildCopilotQuota(user, serverOffsetMs)

	s.quotaMu.Lock()
	s.quotaEntries[cacheKey] = quotaCacheEntry{fingerprint: fingerprint, expiresAt: now.Add(quotaCacheTTL), response: response}
	s.quotaMu.Unlock()
	return response, nil
}

// ResetQuota is not supported: Copilot allowances reset on GitHub's monthly schedule.
func (s *Service) ResetQuota() QuotaResetResponse {
	return QuotaResetResponse{Success: false, Message: "GitHub Copilot quota cannot be reset from the proxy"}
}

func (s *Service) fetchCopilotUser(ctx context.Context, callbackID, githubToken string) (copilotUser, int64, error) {
	cfg := s.Config()
	resp, errDo := s.host.Do(ctx, callbackID, transport.Request{
		Method: http.MethodGet,
		URL:    cfg.GitHubAPIURL + copilotUserEndpoint,
		Headers: http.Header{
			"Accept":               []string{"application/vnd.github+json"},
			"Authorization":        []string{"token " + githubToken},
			"User-Agent":           []string{userAgent()},
			"X-GitHub-Api-Version": []string{"2025-04-01"},
		},
	})
	if errDo != nil {
		return copilotUser{}, 0, fmt.Errorf("fetch Copilot account: %w", errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return copilotUser{}, 0, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, githubToken))
	}
	var user copilotUser
	if errUnmarshal := json.Unmarshal(resp.Body, &user); errUnmarshal != nil {
		return copilotUser{}, 0, fmt.Errorf("decode Copilot account response: %w", errUnmarshal)
	}
	var serverOffsetMs int64
	if dateHeader := resp.Headers.Get("Date"); dateHeader != "" {
		if parsed, errDate := http.ParseTime(dateHeader); errDate == nil {
			serverOffsetMs = parsed.Sub(s.now()).Milliseconds()
		}
	}
	return user, serverOffsetMs, nil
}

var quotaOrder = map[string]int{"premium_interactions": 0, "chat": 1, "completions": 2}

var quotaLabels = map[string]string{
	"premium_interactions": "Premium requests",
	"chat":                 "Chat messages",
	"completions":          "Code completions",
}

func buildCopilotQuota(user copilotUser, serverOffsetMs int64) QuotaFetchResponse {
	out := QuotaFetchResponse{ServerTimeOffsetMs: serverOffsetMs}
	if plan := strings.TrimSpace(user.CopilotPlan); plan != "" {
		out.Subscription = &QuotaSubscription{Plan: plan, TierName: copilotPlanName(plan), TierID: strings.TrimSpace(user.AccessTypeSKU)}
	}
	resetTime := copilotResetTime(user)

	keys := make([]string, 0, len(user.QuotaSnapshots))
	for key := range user.QuotaSnapshots {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		orderI, okI := quotaOrder[keys[i]]
		orderJ, okJ := quotaOrder[keys[j]]
		if okI != okJ {
			return okI
		}
		if orderI != orderJ {
			return orderI < orderJ
		}
		return keys[i] < keys[j]
	})

	var buckets []QuotaBucket
	var summary []QuotaMetric
	for _, key := range keys {
		snapshot := user.QuotaSnapshots[key]
		label := quotaLabels[key]
		if label == "" {
			label = humanizeQuotaID(firstNonEmpty(snapshot.QuotaID, key))
		}
		if snapshot.HasQuota != nil && !*snapshot.HasQuota {
			continue
		}
		if snapshot.Unlimited {
			continue
		}
		// remaining is the integer GitHub shows; quota_remaining carries the fractional credit balance.
		remaining := snapshot.Remaining
		if remaining <= 0 && snapshot.QuotaRemaining > 0 {
			remaining = snapshot.QuotaRemaining
		}
		entitlement := snapshot.Entitlement
		fraction := snapshot.PercentRemaining / 100
		if entitlement > 0 {
			fraction = remaining / entitlement
		}
		unit := "requests"
		if snapshot.TokenBasedBilling || user.TokenBasedBilling {
			unit = "credits"
		}
		description := fmt.Sprintf("%s: %s of %s %s left", label, formatCount(remaining), formatCount(entitlement), unit)
		if snapshot.OverageCount > 0 {
			description += fmt.Sprintf(", %s overage", formatCount(snapshot.OverageCount))
		}
		buckets = append(buckets, QuotaBucket{
			Window:            key,
			RemainingFraction: clampFraction(fraction),
			ResetTime:         resetTime,
			Description:       description,
		})
		if key == "premium_interactions" {
			summary = append(summary,
				QuotaMetric{Key: "premium_entitlement", Label: "Premium " + unit + " included", Value: entitlement, Format: "number"},
				QuotaMetric{Key: "premium_remaining", Label: "Premium " + unit + " left", Value: remaining, Format: "number"},
			)
			if snapshot.CreditsUsed > 0 {
				summary = append(summary, QuotaMetric{Key: "premium_used", Label: "Premium " + unit + " used", Value: snapshot.CreditsUsed, Format: "number"})
			} else if entitlement > 0 && entitlement >= remaining {
				summary = append(summary, QuotaMetric{Key: "premium_used", Label: "Premium " + unit + " used", Value: entitlement - remaining, Format: "number"})
			}
			if snapshot.OverageCount > 0 {
				summary = append(summary, QuotaMetric{Key: "premium_overage", Label: "Premium overage", Value: snapshot.OverageCount, Format: "number"})
			}
		}
	}
	if len(buckets) > 0 {
		out.Groups = []QuotaGroup{{DisplayName: "Copilot allowances", Buckets: buckets}}
	}
	out.Summary = summary
	return out
}

func copilotResetTime(user copilotUser) string {
	if value := strings.TrimSpace(user.QuotaResetDateUTC); value != "" {
		if parsed, errParse := time.Parse(time.RFC3339Nano, value); errParse == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
		return value
	}
	if value := strings.TrimSpace(user.QuotaResetDate); value != "" {
		if parsed, errParse := time.Parse("2006-01-02", value); errParse == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
		return value
	}
	return ""
}

func copilotPlanName(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "free":
		return "Copilot Free"
	case "individual":
		return "Copilot Pro"
	case "individual_pro", "pro_plus":
		return "Copilot Pro+"
	case "business":
		return "Copilot Business"
	case "enterprise":
		return "Copilot Enterprise"
	default:
		return "Copilot " + humanizeQuotaID(plan)
	}
}

func humanizeQuotaID(value string) string {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(value)), func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for index, part := range parts {
		if part != "" {
			parts[index] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, " ")
}

func formatCount(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%.1f", value)
}

func clampFraction(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, value))
}
