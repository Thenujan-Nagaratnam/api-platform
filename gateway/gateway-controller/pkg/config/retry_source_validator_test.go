package config

import (
	"testing"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
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
	if err := ValidateAtMostOneRetrySourcePerRoute(2, nil); err == nil {
		t.Fatal("expected an error for two RetrySourcePolicy declarations on one route, got nil")
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_RejectsDeclarationPlusResilienceRetry(t *testing.T) {
	retry := &api.Retry{}
	if err := ValidateAtMostOneRetrySourcePerRoute(1, retry); err == nil {
		t.Fatal("expected an error for a RetrySourcePolicy declaration combined with resilience.retry, got nil")
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_AllowsOneDeclarationAlone(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(1, nil); err != nil {
		t.Errorf("unexpected error for exactly one RetrySourcePolicy declaration: %v", err)
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_AllowsNeither(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(0, nil); err != nil {
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
		"statusCodes": []interface{}{429, 503},
	}
	decl, err := ParseRetrySourceParams(params, "model")
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
	// TODO: RetriableStatusCodes moved to RetryConditions; this test needs updating
	// if len(decl.RetriableStatusCodes) != 2 {
	// 	t.Errorf("RetriableStatusCodes = %v, want [429, 503]", decl.RetriableStatusCodes)
	// }
}

func TestParseRetrySourceParams_RejectsMissingTargets(t *testing.T) {
	_, err := ParseRetrySourceParams(map[string]interface{}{"statusCodes": []interface{}{500}}, "model")
	if err == nil {
		t.Fatal("expected an error for missing 'targets', got nil")
	}
}

func TestParseRetryTriggerParams_ReadsNamedStatusCodesField(t *testing.T) {
	params := map[string]interface{}{
		"tokenPurgeStatusCodes": []interface{}{401},
	}
	decl, err := ParseRetryTriggerParams(params, "tokenPurgeStatusCodes", 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.StatusCodes) != 1 || decl.StatusCodes[0] != 401 {
		t.Errorf("StatusCodes = %v, want [401]", decl.StatusCodes)
	}
	if decl.MinAttempts == nil || *decl.MinAttempts != 2 {
		t.Errorf("MinAttempts = %v, want 2", decl.MinAttempts)
	}
}

// TestParseRetryTriggerParams_RejectsOutOfRangeStatusCode is the I3 regression test: the
// sibling parser, ParseRetrySourceParams, has always rejected a statusCodes value outside
// 100-599 (TestParseRetrySourceParams_BuildsGroupsFromStandardShape's neighbor in intent);
// ParseRetryTriggerParams must reject one too, rather than letting it wrap into a large
// uint32 in buildRetryTriggerRetryPolicy and reach Envoy unvalidated.
func TestParseRetryTriggerParams_RejectsOutOfRangeStatusCode(t *testing.T) {
	params := map[string]interface{}{
		"tokenPurgeStatusCodes": []interface{}{700},
	}
	_, err := ParseRetryTriggerParams(params, "tokenPurgeStatusCodes", 2, nil)
	if err == nil {
		t.Fatal("expected an error for an out-of-range status code, got nil")
	}
}

func TestParseRetryTriggerParams_EmptyFieldIsNotAnError(t *testing.T) {
	// tokenPurgeStatusCodes explicitly set to an empty list (purge-on-reject
	// disabled) is valid — the caller (Task 6) treats an empty
	// StatusCodes as "no trigger contribution", not a parse error.
	// A schema default is passed here too, to prove an EXPLICIT empty list
	// still wins over the default rather than being overridden by it.
	schema := &map[string]interface{}{
		"properties": map[string]interface{}{
			"tokenPurgeStatusCodes": map[string]interface{}{
				"default": []interface{}{401},
			},
		},
	}
	params := map[string]interface{}{"tokenPurgeStatusCodes": []interface{}{}}
	decl, err := ParseRetryTriggerParams(params, "tokenPurgeStatusCodes", 2, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.StatusCodes) != 0 {
		t.Errorf("StatusCodes = %v, want empty", decl.StatusCodes)
	}
}

func TestParseRetryTriggerParams_OmittedFieldFallsBackToSchemaDefault(t *testing.T) {
	// tokenPurgeStatusCodes entirely absent from params (the user relied on
	// the schema's declared default, e.g. [401]) must still contribute that
	// default to route-level retry — gateway-controller's own
	// coerceParamsBySchema never materializes a schema default into an
	// omitted params key, so ParseRetryTriggerParams must fall back itself.
	schema := &map[string]interface{}{
		"properties": map[string]interface{}{
			"tokenPurgeStatusCodes": map[string]interface{}{
				"default": []interface{}{401},
			},
		},
	}
	decl, err := ParseRetryTriggerParams(map[string]interface{}{}, "tokenPurgeStatusCodes", 2, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.StatusCodes) != 1 || decl.StatusCodes[0] != 401 {
		t.Errorf("StatusCodes = %v, want [401] from schema default", decl.StatusCodes)
	}
	if decl.MinAttempts == nil || *decl.MinAttempts != 2 {
		t.Errorf("MinAttempts = %v, want 2", decl.MinAttempts)
	}
}

func TestParseRetryTriggerParams_OmittedFieldWithNilSchemaIsNotAnError(t *testing.T) {
	// A nil schema (e.g. a caller/test with no policy registry wired) must
	// behave like today: an absent field contributes nothing, not a panic.
	decl, err := ParseRetryTriggerParams(map[string]interface{}{}, "tokenPurgeStatusCodes", 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.StatusCodes) != 0 {
		t.Errorf("StatusCodes = %v, want empty", decl.StatusCodes)
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
