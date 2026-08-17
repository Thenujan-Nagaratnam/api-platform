// gateway-controller/pkg/config/retry_conditions_merge_test.go
package config

import (
	"strings"
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

func TestMergeRetryConditions_NumRetries_SingleContributorWins(t *testing.T) {
	five := 5
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{StatusCodes: []int{401}},
		{NumRetries: &five},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 5 {
		t.Errorf("expected the one explicit NumRetries (5) to win, got %v", merged.NumRetries)
	}
}

func TestMergeRetryConditions_NumRetries_ConflictingValuesRejected(t *testing.T) {
	two, five := 2, 5
	_, err := MergeRetryConditions([]policy.RetryConditions{
		{NumRetries: &two},
		{NumRetries: &five},
	})
	if err == nil {
		t.Fatal("expected an error for two contributors declaring different exact NumRetries")
	}
	if !strings.Contains(err.Error(), "NumRetries") {
		t.Errorf("expected error to mention NumRetries, got: %v", err)
	}
}

func TestMergeRetryConditions_NumRetries_IdenticalValuesStillAllowed(t *testing.T) {
	// Two contributors independently asking for the SAME exact count is not
	// an ownership conflict — nothing is ambiguous about what the route
	// should do. Unlike BackOff (see below), value equality here is a
	// legitimate way to avoid a spurious rejection.
	three := 3
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{NumRetries: &three},
		{NumRetries: &three},
	})
	if err != nil {
		t.Fatalf("unexpected error for two contributors agreeing on NumRetries: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 3 {
		t.Errorf("expected NumRetries 3, got %v", merged.NumRetries)
	}
}

func TestMergeRetryConditions_BackOff_TwoContributorsAlwaysRejected(t *testing.T) {
	// Unlike NumRetries, BackOff conflicts are rejected even when both
	// contributors set identical values — ownership ambiguity is the
	// problem, not the value.
	bo := policy.RetryBackOff{BaseInterval: 100 * time.Millisecond}
	_, err := MergeRetryConditions([]policy.RetryConditions{
		{BackOff: &bo},
		{BackOff: &bo},
	})
	if err == nil {
		t.Fatal("expected an error when two contributors both set BackOff, even identically")
	}
	if !strings.Contains(err.Error(), "BackOff") {
		t.Errorf("expected error to mention BackOff, got: %v", err)
	}
}

func TestMergeRetryConditions_NumRetries_DerivedFromMinAttemptsWhenUnset(t *testing.T) {
	four := 4
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{MinAttempts: &four},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 3 {
		t.Errorf("expected NumRetries derived as MinAttempts-1 (3), got %v", merged.NumRetries)
	}
}
