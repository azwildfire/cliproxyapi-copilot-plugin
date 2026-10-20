package provider

import (
	"encoding/json"
	"math"
	"testing"
)

const sampleCopilotUser = `{"login":"azwildfire","access_type_sku":"copilot_enterprise_seat_multi_quota","copilot_plan":"enterprise","quota_reset_date":"2026-11-01","quota_reset_date_utc":"2026-11-01T00:00:00.000Z","token_based_billing":true,"quota_snapshots":{"chat":{"overage_count":0,"overage_permitted":false,"percent_remaining":100.0,"quota_id":"chat","quota_remaining":0.0,"unlimited":true,"has_quota":true,"token_based_billing":true,"credits_used":0,"remaining":0,"entitlement":0},"completions":{"percent_remaining":100.0,"quota_id":"completions","unlimited":true,"has_quota":true,"remaining":0,"entitlement":0},"premium_interactions":{"overage_count":0,"overage_permitted":true,"percent_remaining":98.4,"quota_id":"premium_interactions","quota_remaining":984804.6,"unlimited":false,"has_quota":true,"token_based_billing":true,"credits_used":15195,"remaining":984804,"entitlement":1000000}}}`

const sampleFreeUser = `{"login":"someone","copilot_plan":"free","quota_reset_date":"2026-11-01","quota_snapshots":{"chat":{"entitlement":50,"remaining":12,"percent_remaining":24.0,"quota_id":"chat","unlimited":false},"completions":{"entitlement":2000,"remaining":1500,"percent_remaining":75.0,"quota_id":"completions","unlimited":false},"premium_interactions":{"entitlement":50,"remaining":0,"percent_remaining":0,"quota_id":"premium_interactions","unlimited":false,"overage_count":3}}}`

func TestBuildCopilotQuotaEnterprise(t *testing.T) {
	var user copilotUser
	if err := json.Unmarshal([]byte(sampleCopilotUser), &user); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	quota := buildCopilotQuota(user, 120)
	if quota.Subscription == nil || quota.Subscription.Plan != "enterprise" || quota.Subscription.TierName != "Copilot Enterprise" {
		t.Fatalf("subscription %+v", quota.Subscription)
	}
	if quota.ServerTimeOffsetMs != 120 {
		t.Fatalf("server offset %d", quota.ServerTimeOffsetMs)
	}
	if len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 1 {
		t.Fatalf("unlimited chat/completions must be skipped; groups %+v", quota.Groups)
	}
	bucket := quota.Groups[0].Buckets[0]
	if bucket.Window != "premium_interactions" || math.Abs(bucket.RemainingFraction-0.984804) > 1e-6 {
		t.Fatalf("bucket %+v", bucket)
	}
	if bucket.ResetTime != "2026-11-01T00:00:00Z" {
		t.Fatalf("reset time %q", bucket.ResetTime)
	}
	metrics := map[string]float64{}
	for _, metric := range quota.Summary {
		metrics[metric.Key] = metric.Value
	}
	if metrics["premium_entitlement"] != 1000000 || metrics["premium_remaining"] != 984804 || metrics["premium_used"] != 15195 {
		t.Fatalf("summary %+v", quota.Summary)
	}
	raw, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if _, ok := decoded["groups"]; !ok {
		t.Fatalf("groups missing: %s", raw)
	}
}

func TestBuildCopilotQuotaFreePlan(t *testing.T) {
	var user copilotUser
	if err := json.Unmarshal([]byte(sampleFreeUser), &user); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	quota := buildCopilotQuota(user, 0)
	if quota.Subscription == nil || quota.Subscription.TierName != "Copilot Free" {
		t.Fatalf("subscription %+v", quota.Subscription)
	}
	if len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 3 {
		t.Fatalf("free plan should meter all three quotas: %+v", quota.Groups)
	}
	first := quota.Groups[0].Buckets[0]
	if first.Window != "premium_interactions" || first.RemainingFraction != 0 {
		t.Fatalf("premium bucket should come first and be exhausted: %+v", first)
	}
	if quota.Groups[0].Buckets[1].Window != "chat" || math.Abs(quota.Groups[0].Buckets[1].RemainingFraction-0.24) > 1e-9 {
		t.Fatalf("chat bucket %+v", quota.Groups[0].Buckets[1])
	}
	if first.ResetTime != "2026-11-01T00:00:00Z" {
		t.Fatalf("date-only reset must become RFC3339: %q", first.ResetTime)
	}
	overage := 0.0
	for _, metric := range quota.Summary {
		if metric.Key == "premium_overage" {
			overage = metric.Value
		}
	}
	if overage != 3 {
		t.Fatalf("overage metric missing: %+v", quota.Summary)
	}
}

func TestCopilotPlanName(t *testing.T) {
	cases := map[string]string{"individual": "Copilot Pro", "individual_pro": "Copilot Pro+", "business": "Copilot Business", "something_new": "Copilot Something New"}
	for plan, want := range cases {
		if got := copilotPlanName(plan); got != want {
			t.Errorf("%s: %q, want %q", plan, got, want)
		}
	}
}
