// gateway-controller/pkg/config/retry_conditions_merge_test.go
package config

import (
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestMergeRetryConditions_UnionFields(t *testing.T) {
	contributions := []policy.RetryConditions{
		{On: []string{"retriable-status-codes"}, StatusCodes: []int{401}},
		{On: []string{"reset"}, StatusCodes: []int{403, 401}}, // 401 duplicated on purpose
		{Headers: []policy.RetriableHeader{{Name: "x-retry-me", Exact: "true"}}},
	}

	merged, err := MergeRetryConditions(contributions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantOn := map[string]bool{"retriable-status-codes": true, "reset": true, "retriable-headers": true}
	if len(merged.On) != len(wantOn) {
		t.Fatalf("expected On %v, got %v", wantOn, merged.On)
	}
	for _, on := range merged.On {
		if !wantOn[on] {
			t.Errorf("unexpected On condition %q", on)
		}
	}

	wantCodes := map[int]bool{401: true, 403: true}
	if len(merged.StatusCodes) != len(wantCodes) {
		t.Fatalf("expected deduplicated StatusCodes %v, got %v", wantCodes, merged.StatusCodes)
	}
	for _, code := range merged.StatusCodes {
		if !wantCodes[code] {
			t.Errorf("unexpected status code %d", code)
		}
	}

	if len(merged.Headers) != 1 || merged.Headers[0].Name != "x-retry-me" {
		t.Fatalf("expected the one declared header to survive, got %v", merged.Headers)
	}
}

func TestMergeRetryConditions_StatusCodesImpliesRetriableStatusCodesOn(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{{StatusCodes: []int{401}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.On) != 1 || merged.On[0] != "retriable-status-codes" {
		t.Errorf("expected StatusCodes to imply On=[retriable-status-codes], got %v", merged.On)
	}
}

func TestMergeRetryConditions_HeadersImpliesRetriableHeadersOn(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{Headers: []policy.RetriableHeader{{Name: "x-retry-me", Exact: "true"}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.On) != 1 || merged.On[0] != "retriable-headers" {
		t.Errorf("expected Headers to imply On=[retriable-headers], got %v", merged.On)
	}
}

func TestMergeRetryConditions_AvoidPreviousHosts_OR(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{AvoidPreviousHosts: false},
		{AvoidPreviousHosts: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !merged.AvoidPreviousHosts {
		t.Error("expected AvoidPreviousHosts to be true when any contributor sets it")
	}
}

func TestMergeRetryConditions_MinAttempts_MaxFloor(t *testing.T) {
	two, three := 2, 3
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{MinAttempts: &two},
		{MinAttempts: &three},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.MinAttempts == nil || *merged.MinAttempts != 3 {
		t.Errorf("expected MinAttempts to be raised to the max (3), got %v", merged.MinAttempts)
	}
}

func TestMergeRetryConditions_PerTryTimeout_MinCeiling(t *testing.T) {
	fiveSec, tenSec := 5*time.Second, 10*time.Second
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{PerTryTimeout: &tenSec},
		{PerTryTimeout: &fiveSec},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.PerTryTimeout == nil || *merged.PerTryTimeout != fiveSec {
		t.Errorf("expected PerTryTimeout tightened to the min (5s), got %v", merged.PerTryTimeout)
	}
}
