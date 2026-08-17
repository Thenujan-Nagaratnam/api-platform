/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package xds

import (
	"testing"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestRetrySourceTargetClusterNames_MainAndNamedTargets(t *testing.T) {
	group := policy.RetryGroup{
		Key: "gpt-4o",
		OrderedTargets: []policy.RetryTarget{
			{UpstreamDefinitionName: ""},       // main
			{UpstreamDefinitionName: "backup"}, // named
		},
	}
	got := retrySourceTargetClusterNames(group, "LlmProvider", "abc-123", "upstream_main_example.com_443")
	want := []string{
		"upstream_main_example.com_443",
		"upstream_LlmProvider_abc-123_backup",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRetrySourceAggregateClusterKey_MatchesSDKFormula(t *testing.T) {
	got := RetrySourceAggregateClusterKey("LlmProvider", "abc-123", "POST|/chat/completions|main", "gpt-4o")
	// Must be built from the SAME local-name formula policy.RetrySourceUpstreamName
	// produces, so a policy setting UpstreamName at runtime resolves to
	// exactly this cluster.
	wantLocal := policy.RetrySourceUpstreamName("POST|/chat/completions|main", "gpt-4o")
	want := "upstream_LlmProvider_abc-123_" + wantLocal
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// retryTestDefinitions builds a policy-definition registry in the real
// "name|fullVersion" key format pkg/config uses, declaring one retry-source
// policy and one retry-trigger policy. Nothing here names model-failover or
// oauth2-generator specifically — discovery is driven purely by the declared
// metadata, which is the whole point of this mechanism.
func retryTestDefinitions() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"src-policy|v1.0.0": {
			Name:        "src-policy",
			Version:     "v1.0.0",
			RetrySource: &models.RetrySourceMetadata{GroupKeyField: "model"},
		},
		"trigger-policy|v1.0.0": {
			Name:         "trigger-policy",
			Version:      "v1.0.0",
			RetryTrigger: &models.RetryTriggerMetadata{StatusCodesField: "purgeStatusCodes", MinAttempts: 2},
		},
		"inert-policy|v1.0.0": {
			Name:    "inert-policy",
			Version: "v1.0.0",
		},
	}
}

func newRetryTestTranslator() *Translator {
	tr := NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig())
	tr.SetPolicyDefinitions(retryTestDefinitions())
	return tr
}

// A policy is discovered by its declared x-wso2-retry-source metadata, never by
// its name — the whole point of this task. The chain carries a major-only
// version ("v1"), exactly as pkg/transform writes it, so the lookup must
// resolve it to the full version before keying the registry.
func TestResolveRetryDeclarations_DiscoversRetrySourceByMetadata(t *testing.T) {
	tr := newRetryTestTranslator()
	chain := &models.PolicyChain{Policies: []models.Policy{{
		Name:    "src-policy",
		Version: "v1",
		Params: map[string]interface{}{
			"targets": []interface{}{
				map[string]interface{}{
					"model":     "gpt-4o",
					"fallbacks": []interface{}{map[string]interface{}{"upstreamDefinition": "backup"}},
				},
			},
			"statusCodes": []interface{}{429},
		},
	}}}

	decl, count, triggerCodes, triggerMinAttempts, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("sourceCount = %d, want 1", count)
	}
	if decl == nil {
		t.Fatal("expected a retry-source declaration, got nil")
	}
	if len(decl.Groups) != 1 || decl.Groups[0].Key != "gpt-4o" {
		t.Errorf("groups = %+v, want one group keyed gpt-4o", decl.Groups)
	}
	if len(decl.Groups[0].OrderedTargets) != 2 {
		t.Errorf("orderedTargets = %+v, want 2 (main + backup)", decl.Groups[0].OrderedTargets)
	}
	if len(triggerCodes) != 0 {
		t.Errorf("triggerCodes = %v, want empty", triggerCodes)
	}
	if triggerMinAttempts != 0 {
		t.Errorf("triggerMinAttempts = %d, want 0", triggerMinAttempts)
	}
}

func TestResolveRetryDeclarations_DiscoversRetryTriggerByMetadata(t *testing.T) {
	tr := newRetryTestTranslator()
	chain := &models.PolicyChain{Policies: []models.Policy{{
		Name:    "trigger-policy",
		Version: "v1",
		Params:  map[string]interface{}{"purgeStatusCodes": []interface{}{401, 403}},
	}}}

	decl, count, triggerCodes, triggerMinAttempts, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl != nil || count != 0 {
		t.Errorf("expected no retry-source, got decl=%+v count=%d", decl, count)
	}
	for _, code := range []int{401, 403} {
		if _, ok := triggerCodes[code]; !ok {
			t.Errorf("triggerCodes missing %d, got %v", code, triggerCodes)
		}
	}
	if triggerMinAttempts != 2 {
		t.Errorf("triggerMinAttempts = %d, want 2", triggerMinAttempts)
	}
}

// A policy whose definition declares neither must contribute nothing at all —
// this is what keeps every ordinary policy in a chain out of the retry path.
func TestResolveRetryDeclarations_IgnoresPolicyWithNoRetryMetadata(t *testing.T) {
	tr := newRetryTestTranslator()
	chain := &models.PolicyChain{Policies: []models.Policy{
		{Name: "inert-policy", Version: "v1", Params: map[string]interface{}{"statusCodes": []interface{}{500}}},
		{Name: "unregistered-policy", Version: "v1"},
	}}

	decl, count, triggerCodes, _, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl != nil || count != 0 || len(triggerCodes) != 0 {
		t.Errorf("expected nothing discovered, got decl=%+v count=%d codes=%v", decl, count, triggerCodes)
	}
}

// Composition: a trigger policy's codes fold into the retry-source's own
// RetryPolicy rather than producing a second, conflicting one.
func TestResolveRetryDeclarations_SourceAndTriggerCompose(t *testing.T) {
	tr := newRetryTestTranslator()
	chain := &models.PolicyChain{Policies: []models.Policy{
		{
			Name:    "src-policy",
			Version: "v1",
			Params: map[string]interface{}{
				"targets": []interface{}{map[string]interface{}{
					"model":     "gpt-4o",
					"fallbacks": []interface{}{map[string]interface{}{"upstreamDefinition": "backup"}},
				}},
				"statusCodes": []interface{}{429},
			},
		},
		{
			Name:    "trigger-policy",
			Version: "v1",
			Params:  map[string]interface{}{"purgeStatusCodes": []interface{}{401}},
		},
	}}

	decl, count, triggerCodes, _, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 || decl == nil {
		t.Fatalf("expected exactly one retry-source, got count=%d decl=%+v", count, decl)
	}
	if _, ok := triggerCodes[401]; !ok {
		t.Errorf("triggerCodes = %v, want 401 present", triggerCodes)
	}
	merged := mergeRetriableStatusCodes(decl.RetriableStatusCodes, triggerCodes)
	if len(merged) != 2 {
		t.Errorf("merged codes = %v, want both 429 and 401", merged)
	}
}

// The new capability: a route carrying ONLY retry-trigger declarations gets a
// plain, non-aggregate RouteAction.RetryPolicy with no RetryPriority — ordinary
// same-cluster Envoy retry, with no retry-source policy present at all.
func TestBuildRetryTriggerRetryPolicy_PlainNoRetryPriority(t *testing.T) {
	rp := buildRetryTriggerRetryPolicy(map[int]struct{}{401: {}}, 2)
	if rp == nil {
		t.Fatal("expected a retry policy")
	}
	if rp.RetryPriority != nil {
		t.Error("plain trigger-only retry must not set RetryPriority (no aggregate cluster exists)")
	}
	if rp.RetryOn != "retriable-status-codes" {
		t.Errorf("RetryOn = %q", rp.RetryOn)
	}
	if rp.GetNumRetries().GetValue() != 1 {
		t.Errorf("NumRetries = %d, want 1 (minAttempts 2 - 1)", rp.GetNumRetries().GetValue())
	}
	if len(rp.RetriableStatusCodes) != 1 || rp.RetriableStatusCodes[0] != 401 {
		t.Errorf("RetriableStatusCodes = %v, want [401]", rp.RetriableStatusCodes)
	}
}

func TestBuildRetryTriggerRetryPolicy_NoCodesYieldsNoPolicy(t *testing.T) {
	if rp := buildRetryTriggerRetryPolicy(map[int]struct{}{}, 2); rp != nil {
		t.Errorf("expected nil retry policy with no trigger codes, got %+v", rp)
	}
}

// The aggregate-cluster RetryPolicy keeps model-failover's existing shape:
// RetryPriority set, NumRetries from the longest group chain, PerTryTimeout
// from the declaration — just derived generically now.
func TestBuildRetrySourceRetryPolicy_UsesLongestGroupChain(t *testing.T) {
	decl := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "a", OrderedTargets: []policy.RetryTarget{{}, {UpstreamDefinitionName: "b"}}},
			{Key: "c", OrderedTargets: []policy.RetryTarget{{}, {UpstreamDefinitionName: "d"}, {UpstreamDefinitionName: "e"}}},
		},
		RetriableStatusCodes: []int{429},
	}
	rp := buildRetrySourceRetryPolicy(decl, 0)
	if rp.RetryPriority == nil {
		t.Error("aggregate-cluster retry must set RetryPriority")
	}
	if rp.GetNumRetries().GetValue() != 2 {
		t.Errorf("NumRetries = %d, want 2 (longest chain 3 targets - 1)", rp.GetNumRetries().GetValue())
	}
}

// TestCreateRouteFromRDC_TriggerComposesWithResilienceRetry is the I1 regression test (see the
// 2026-08-14 final review): a retry-trigger policy must UNION its status codes and NumRetries
// floor into an operator's configured resilience.retry, never replace it — unlike a retry
// source, a retry trigger is designed to never conflict with anything.
func TestCreateRouteFromRDC_TriggerComposesWithResilienceRetry(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "GET|/api/v1.0/items|"
	numRetries := 3
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main": {Endpoints: []models.Endpoint{{Host: "echo", Port: 80}}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			routeKey: {Policies: []models.Policy{{
				Name: "trigger-policy", Version: "v1",
				Params: map[string]interface{}{"purgeStatusCodes": []interface{}{401}},
			}}},
		},
	}
	rdcRoute := &models.Route{
		Method:        "GET",
		Path:          "/api/v1.0/items",
		OperationPath: "/items",
		Timeout:       &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{502, 503}, NumRetries: &numRetries}},
		Upstream:      models.RouteUpstream{ClusterKey: "main"},
	}

	r := tr.createRouteFromRDC(routeKey, rdcRoute, rdc)
	if r == nil {
		t.Fatal("expected a route")
	}
	rp := r.GetRoute().GetRetryPolicy()
	if rp == nil {
		t.Fatal("expected a RetryPolicy")
	}
	wantCodes := map[uint32]bool{502: true, 503: true, 401: true}
	if len(rp.RetriableStatusCodes) != len(wantCodes) {
		t.Fatalf("RetriableStatusCodes = %v, want the union of %v", rp.RetriableStatusCodes, wantCodes)
	}
	for _, code := range rp.RetriableStatusCodes {
		if !wantCodes[code] {
			t.Errorf("unexpected status code %d in merged RetryPolicy", code)
		}
	}
	// NumRetries must stay at the operator's configured 3 — the trigger's minAttempts-1 (1)
	// is only ever a floor, never a ceiling on the operator's own value.
	if got := rp.GetNumRetries().GetValue(); got != 3 {
		t.Errorf("NumRetries = %d, want 3 (operator's resilience.retry, not clobbered by the trigger)", got)
	}
}

// TestCreateRouteFromRDC_ZeroFallbackSourcePlusTriggerGetsNonZeroNumRetries is the I2
// regression test: a retry-source declaration whose every group is single-target (legal — no
// fallbacks at all, maxChain==0) combined with a retry-trigger must still get a non-zero retry
// budget, or the merged trigger status code can never actually fire.
func TestCreateRouteFromRDC_ZeroFallbackSourcePlusTriggerGetsNonZeroNumRetries(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "POST|/mf-test/latest/chat/completions|"
	sourceParams := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{"model": "gpt-4o", "upstreamDefinition": "primary"}, // no fallbacks
		},
		"statusCodes": []interface{}{500},
	}
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{},
		PolicyChains: map[string]*models.PolicyChain{
			routeKey: {Policies: []models.Policy{
				{Name: "src-policy", Version: "v1", Params: sourceParams},
				{Name: "trigger-policy", Version: "v1", Params: map[string]interface{}{"purgeStatusCodes": []interface{}{401}}},
			}},
		},
	}
	rdcRoute := &models.Route{
		Method:        "POST",
		Path:          "/mf-test/latest/chat/completions",
		OperationPath: "/chat/completions",
		Upstream:      models.RouteUpstream{UseClusterHeader: true, DefaultCluster: "upstream_LlmProvider_test-uuid_main"},
	}

	r := tr.createRouteFromRDC(routeKey, rdcRoute, rdc)
	if r == nil {
		t.Fatal("expected a route")
	}
	rp := r.GetRoute().GetRetryPolicy()
	if rp == nil {
		t.Fatal("expected a RetryPolicy")
	}
	// maxChain is 0 (no fallbacks); the trigger's minAttempts (2, from retryTestDefinitions)
	// must raise NumRetries to 1, or the merged 401 code would never get a chance to retry.
	if got := rp.GetNumRetries().GetValue(); got != 1 {
		t.Errorf("NumRetries = %d, want 1 (maxChain 0, raised to triggerMinAttempts-1)", got)
	}
	wantCodes := map[uint32]bool{500: true, 401: true}
	if len(rp.RetriableStatusCodes) != len(wantCodes) {
		t.Fatalf("RetriableStatusCodes = %v, want the union of %v", rp.RetriableStatusCodes, wantCodes)
	}
	for _, code := range rp.RetriableStatusCodes {
		if !wantCodes[code] {
			t.Errorf("unexpected status code %d in merged RetryPolicy", code)
		}
	}
}
