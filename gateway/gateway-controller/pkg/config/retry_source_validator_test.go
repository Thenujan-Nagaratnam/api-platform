package config

import (
	"testing"
	"time"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestValidateRetrySourceTargetsHaveNoBasePath_RejectsGroupWithFallbacksAndBasePath(t *testing.T) {
	decl := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "aliased-provider"},
				{UpstreamDefinitionName: "fallback"},
			}},
		},
	}
	basePathByUpstreamDef := map[string]string{"aliased-provider": "/anthropic-ctx"}
	err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, "")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestValidateRetrySourceTargetsHaveNoBasePath_AllowsSingleTargetGroup(t *testing.T) {
	decl := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{{UpstreamDefinitionName: "aliased-provider"}}},
		},
	}
	basePathByUpstreamDef := map[string]string{"aliased-provider": "/anthropic-ctx"}
	if err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, ""); err != nil {
		t.Errorf("unexpected error for a single-target (no-failover) group: %v", err)
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_RejectsTwoDeclarations(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(2); err == nil {
		t.Fatal("expected an error for two RetrySourcePolicy declarations on one route, got nil")
	}
}

// A retry-source policy coexisting with an operator's resilience.retry is deliberately NO
// LONGER rejected here — the two compose field-by-field via MergeRetryConditions, which
// rejects only a real NumRetries/BackOff ownership conflict. This validator now enforces
// exactly one rule: the singular RetryPriority slot on Envoy's RetryPolicy proto.
func TestValidateAtMostOneRetrySourcePerRoute_AllowsOneDeclarationAlone(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(1); err != nil {
		t.Errorf("unexpected error for exactly one RetrySourcePolicy declaration: %v", err)
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_AllowsNeither(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(0); err != nil {
		t.Errorf("unexpected error when nothing declares retry ownership: %v", err)
	}
}

func TestParseRetrySourceParams_BuildsGroupsFromStandardShape(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":              "gpt-4o",
				"upstreamDefinition": "gpt4-primary",
				"fallbacks": []interface{}{
					map[string]interface{}{"upstreamDefinition": "gpt4-fallback"},
				},
			},
			map[string]interface{}{
				"model":              "claude-3",
				"upstreamDefinition": "claude-primary",
			},
		},
		// statusCodes is deliberately still present in params: ParseRetrySourceParams must
		// simply IGNORE it now (retriable status codes travel through the policy's own
		// x-wso2-retry-conditions block instead), not error on it and not read it.
		"statusCodes": []interface{}{429, 503},
	}
	decl, err := ParseRetrySourceParams(params, "model", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.Groups) != 2 {
		t.Fatalf("Groups = %d, want 2", len(decl.Groups))
	}
	if decl.Groups[0].Key != "gpt-4o" {
		t.Errorf("Groups[0].Key = %q, want %q", decl.Groups[0].Key, "gpt-4o")
	}
	if len(decl.Groups[0].OrderedTargets) != 2 {
		t.Fatalf("Groups[0].OrderedTargets = %d, want 2", len(decl.Groups[0].OrderedTargets))
	}
	if decl.Groups[0].OrderedTargets[0].UpstreamDefinitionName != "gpt4-primary" {
		t.Errorf("OrderedTargets[0] = %q, want %q", decl.Groups[0].OrderedTargets[0].UpstreamDefinitionName, "gpt4-primary")
	}
	if decl.Groups[0].OrderedTargets[1].UpstreamDefinitionName != "gpt4-fallback" {
		t.Errorf("OrderedTargets[1] = %q, want %q", decl.Groups[0].OrderedTargets[1].UpstreamDefinitionName, "gpt4-fallback")
	}
	if len(decl.Groups[1].OrderedTargets) != 1 {
		t.Errorf("Groups[1].OrderedTargets = %d, want 1 (no fallbacks declared)", len(decl.Groups[1].OrderedTargets))
	}
}

// additionalProvider is the generic, cross-provider counterpart to upstreamDefinition — see
// gateway/spec/prds/llm-cross-provider-failover.md. This parser reads it the same way it
// already reads upstreamDefinition: a fixed key name in the shared structural shape, not
// itself configurable via groupKeyField/targetsField, so ANY policy declaring
// x-wso2-retry-source gets it for free, not just model-failover.
func TestParseRetrySourceParams_ReadsAdditionalProviderReference(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":              "gpt-4o",
				"upstreamDefinition": "primary",
				"fallbacks": []interface{}{
					map[string]interface{}{"upstreamDefinition": "fallback-1"},
					map[string]interface{}{"additionalProvider": "anthropic-backup"},
				},
			},
		},
	}
	decl, err := ParseRetrySourceParams(params, "model", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.Groups) != 1 || len(decl.Groups[0].OrderedTargets) != 3 {
		t.Fatalf("Groups = %+v, want 1 group with 3 ordered targets", decl.Groups)
	}
	primary := decl.Groups[0].OrderedTargets[0]
	if primary.UpstreamDefinitionName != "primary" || primary.AdditionalProviderName != "" {
		t.Errorf("OrderedTargets[0] = %+v, want plain upstreamDefinition 'primary'", primary)
	}
	fb1 := decl.Groups[0].OrderedTargets[1]
	if fb1.UpstreamDefinitionName != "fallback-1" || fb1.AdditionalProviderName != "" {
		t.Errorf("OrderedTargets[1] = %+v, want plain upstreamDefinition 'fallback-1'", fb1)
	}
	fb2 := decl.Groups[0].OrderedTargets[2]
	if fb2.AdditionalProviderName != "anthropic-backup" || fb2.UpstreamDefinitionName != "" {
		t.Errorf("OrderedTargets[2] = %+v, want AdditionalProviderName 'anthropic-backup'", fb2)
	}
}

// A target/fallback entry declaring BOTH upstreamDefinition and additionalProvider is
// ambiguous — the two are mutually exclusive reference kinds (see RetryTarget's own doc
// comment in the SDK) and must be rejected outright, not silently resolved by picking one.
func TestParseRetrySourceParams_RejectsBothUpstreamDefinitionAndAdditionalProvider(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":              "gpt-4o",
				"upstreamDefinition": "primary",
				"fallbacks": []interface{}{
					map[string]interface{}{
						"upstreamDefinition": "fallback-1",
						"additionalProvider": "anthropic-backup",
					},
				},
			},
		},
	}
	if _, err := ParseRetrySourceParams(params, "model", ""); err == nil {
		t.Fatal("expected an error for a fallback declaring both upstreamDefinition and additionalProvider, got nil")
	}
}

// The primary target[i] entry (not just fallbacks[]) can also reference an additionalProvider
// — nothing in the shape restricts additionalProvider to fallback-only.
func TestParseRetrySourceParams_AdditionalProviderOnPrimaryTarget(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":              "claude-3-5-sonnet",
				"additionalProvider": "anthropic-primary",
			},
		},
	}
	decl, err := ParseRetrySourceParams(params, "model", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.Groups) != 1 || len(decl.Groups[0].OrderedTargets) != 1 {
		t.Fatalf("Groups = %+v, want 1 group with 1 ordered target", decl.Groups)
	}
	got := decl.Groups[0].OrderedTargets[0]
	if got.AdditionalProviderName != "anthropic-primary" || got.UpstreamDefinitionName != "" {
		t.Errorf("OrderedTargets[0] = %+v, want AdditionalProviderName 'anthropic-primary'", got)
	}
}

func TestParseRetrySourceParams_RejectsMissingTargets(t *testing.T) {
	_, err := ParseRetrySourceParams(map[string]interface{}{"statusCodes": []interface{}{500}}, "model", "")
	if err == nil {
		t.Fatal("expected an error for missing 'targets', got nil")
	}
}

// A policy whose x-wso2-retry-source names a non-default targetsField must have its ordered
// target list read from THAT field, and a plain "targets" key must not satisfy the
// requirement in its place.
func TestParseRetrySourceParams_HonoursCustomTargetsField(t *testing.T) {
	params := map[string]interface{}{
		"providers": []interface{}{
			map[string]interface{}{
				"model":              "gpt-4o",
				"upstreamDefinition": "primary",
				"fallbacks":          []interface{}{map[string]interface{}{"upstreamDefinition": "secondary"}},
			},
		},
		"targets": []interface{}{
			map[string]interface{}{"model": "decoy", "upstreamDefinition": "decoy-upstream"},
		},
	}
	decl, err := ParseRetrySourceParams(params, "model", "providers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.Groups) != 1 || decl.Groups[0].Key != "gpt-4o" {
		t.Fatalf("Groups = %+v, want one group keyed gpt-4o read from 'providers'", decl.Groups)
	}
	if len(decl.Groups[0].OrderedTargets) != 2 {
		t.Errorf("OrderedTargets = %+v, want 2 (primary + secondary)", decl.Groups[0].OrderedTargets)
	}
}

func TestParseRetrySourceParams_CustomTargetsFieldMissingIsRejected(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{map[string]interface{}{"model": "gpt-4o"}},
	}
	if _, err := ParseRetrySourceParams(params, "model", "providers"); err == nil {
		t.Fatal("expected an error when the declared targetsField is absent, got nil")
	}
}

// requestTimeout stays part of the retry-source shape (it bounds ONE attempt on this
// policy's own failover chain, Envoy's PerTryTimeout) even though status codes moved out.
func TestParseRetrySourceParams_ReadsRequestTimeout(t *testing.T) {
	params := map[string]interface{}{
		"targets":        []interface{}{map[string]interface{}{"model": "gpt-4o"}},
		"requestTimeout": "10s",
	}
	decl, err := ParseRetrySourceParams(params, "model", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl.PerAttemptTimeout == nil || *decl.PerAttemptTimeout != 10*time.Second {
		t.Errorf("PerAttemptTimeout = %v, want 10s", decl.PerAttemptTimeout)
	}
}

func TestLookupRetryMetadata_ReturnsRetryConditionsBlock(t *testing.T) {
	conditions := map[string]interface{}{"statusCodes": []interface{}{401}}
	definitions := map[string]models.PolicyDefinition{
		"test-policy|v1.0.0": {
			Name:            "test-policy",
			Version:         "v1.0.0",
			RetryConditions: &conditions,
		},
	}
	latestVersions := map[string]string{"test-policy": "v1.0.0"}

	_, _, gotConditions := LookupRetryMetadata(definitions, latestVersions, "test-policy", "v1")
	if gotConditions == nil {
		t.Fatal("expected the retry-conditions block to be returned")
	}
	if len(*gotConditions) != 1 {
		t.Errorf("expected one key in the retry-conditions block, got %v", *gotConditions)
	}
}
