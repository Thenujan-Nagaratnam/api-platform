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
	"time"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
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
// "name|fullVersion" key format pkg/config uses. "src-policy" declares
// x-wso2-retry-source (WHERE to retry to) plus its own x-wso2-retry-conditions
// block pointing at its statusCodes param; "trigger-policy" declares only
// x-wso2-retry-conditions (WHAT to retry on) — the generalized replacement for
// the deleted x-wso2-retry-trigger metadata, expressing the same
// "read my named param, and I need at least 2 attempts" contract. Nothing here
// names model-failover or oauth2-generator specifically — discovery is driven
// purely by the declared metadata, which is the whole point of this mechanism.
func retryTestDefinitions() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"src-policy|v1.0.0": {
			Name:            "src-policy",
			Version:         "v1.0.0",
			RetrySource:     &models.RetrySourceMetadata{GroupKeyField: "model"},
			RetryConditions: &map[string]interface{}{"statusCodes": map[string]interface{}{"fromParam": "statusCodes"}},
		},
		"trigger-policy|v1.0.0": {
			Name:    "trigger-policy",
			Version: "v1.0.0",
			RetryConditions: &map[string]interface{}{
				"statusCodes": map[string]interface{}{"fromParam": "purgeStatusCodes"},
				"minAttempts": 2,
			},
		},
		"inert-policy|v1.0.0": {
			Name:    "inert-policy",
			Version: "v1.0.0",
		},
	}
}

// containsCode reports whether merged retry conditions include a status code.
// Merged StatusCodes come out of a Go map, so tests must never assume order.
func containsCode(codes []int, want int) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
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

	decl, merged, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	// The retry-source policy's own status codes now arrive via its separate
	// x-wso2-retry-conditions block, in the merged conditions — never on the declaration.
	if len(merged.StatusCodes) != 1 || merged.StatusCodes[0] != 429 {
		t.Errorf("merged.StatusCodes = %v, want [429] from the policy's own retry conditions", merged.StatusCodes)
	}
	if merged.MinAttempts != nil {
		t.Errorf("merged.MinAttempts = %v, want nil (nothing declared a floor)", merged.MinAttempts)
	}
}

// A policy declaring ONLY x-wso2-retry-conditions (no retry source) contributes its
// conditions and nothing else — the generalized replacement for the old retry-trigger path.
func TestResolveRetryDeclarations_DiscoversRetryConditionsByMetadata(t *testing.T) {
	tr := newRetryTestTranslator()
	chain := &models.PolicyChain{Policies: []models.Policy{{
		Name:    "trigger-policy",
		Version: "v1",
		Params:  map[string]interface{}{"purgeStatusCodes": []interface{}{401, 403}},
	}}}

	decl, merged, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl != nil {
		t.Errorf("expected no retry-source, got decl=%+v", decl)
	}
	for _, code := range []int{401, 403} {
		if !containsCode(merged.StatusCodes, code) {
			t.Errorf("merged.StatusCodes missing %d, got %v", code, merged.StatusCodes)
		}
	}
	if merged.MinAttempts == nil || *merged.MinAttempts != 2 {
		t.Errorf("merged.MinAttempts = %v, want 2", merged.MinAttempts)
	}
	// MergeRetryConditions derives NumRetries from the MinAttempts floor when no
	// contributor requested an exact count.
	if merged.NumRetries == nil || *merged.NumRetries != 1 {
		t.Errorf("merged.NumRetries = %v, want 1 (derived from minAttempts 2)", merged.NumRetries)
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

	decl, merged, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl != nil || len(merged.StatusCodes) != 0 || len(merged.On) != 0 {
		t.Errorf("expected nothing discovered, got decl=%+v merged=%+v", decl, merged)
	}
}

// I1 regression test (2026-08-17 final review): a chain-vs-chain NumRetries ownership
// conflict on ONE route must degrade gracefully — that route loses its retry policy, every
// other route on the same API still translates — never abort translateRuntimeConfig for the
// whole API. Before the fix, translateRuntimeConfig called resolveRetryDeclarations (which
// merges eagerly, purely to decide whether a retry-source aggregate cluster is needed) and
// propagated that merge's conflict error, failing translation of every route and cluster.
// createRouteFromRDC already handles the identical conflict class (including the operator's
// resilience.retry as a contributor) by warning and leaving just that one route with no
// RetryPolicy — this test pins translateRuntimeConfig to the same graceful behavior.
func TestTranslateRuntimeConfig_ChainConflictDegradesOnlyThatRoute(t *testing.T) {
	tr := NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig())
	tr.SetPolicyDefinitions(map[string]models.PolicyDefinition{
		"conflict-a-policy|v1.0.0": {
			Name:            "conflict-a-policy",
			Version:         "v1.0.0",
			RetryConditions: &map[string]interface{}{"statusCodes": []interface{}{500}, "numRetries": 2},
		},
		"conflict-b-policy|v1.0.0": {
			Name:            "conflict-b-policy",
			Version:         "v1.0.0",
			RetryConditions: &map[string]interface{}{"statusCodes": []interface{}{501}, "numRetries": 5},
		},
	})

	conflictRouteKey := "GET|/conflict|"
	otherRouteKey := "GET|/other|"
	rdc := &models.RuntimeDeployConfig{
		Metadata: models.Metadata{Kind: "RestApi", UUID: "api-conflict"},
		Routes: map[string]*models.Route{
			conflictRouteKey: {
				Method: "GET", Path: "/conflict", OperationPath: "/conflict",
				Upstream: models.RouteUpstream{ClusterKey: "main"},
			},
			otherRouteKey: {
				Method: "GET", Path: "/other", OperationPath: "/other",
				Upstream: models.RouteUpstream{ClusterKey: "main"},
			},
		},
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main": {Endpoints: []models.Endpoint{{Host: "echo", Port: 80}}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			// Two chain members, each demanding a different explicit NumRetries —
			// a genuine ownership conflict per MergeRetryConditions.
			conflictRouteKey: {Policies: []models.Policy{
				{Name: "conflict-a-policy", Version: "v1"},
				{Name: "conflict-b-policy", Version: "v1"},
			}},
			// otherRouteKey deliberately has no PolicyChains entry at all — an
			// unrelated route on the same API that must be unaffected.
		},
	}

	routes, _, err := tr.translateRuntimeConfig(rdc)
	if err != nil {
		t.Fatalf("expected translateRuntimeConfig to succeed with the conflicting route gracefully degraded, got error: %v", err)
	}

	var conflictRoute, otherRoute *route.Route
	for _, r := range routes {
		switch r.Name {
		case conflictRouteKey:
			conflictRoute = r
		case otherRouteKey:
			otherRoute = r
		}
	}
	if conflictRoute == nil {
		t.Fatal("expected the conflicting route to still be translated")
	}
	if rp := conflictRoute.GetRoute().GetRetryPolicy(); rp != nil {
		t.Errorf("expected no RetryPolicy on the conflicting route, got %+v", rp)
	}
	if otherRoute == nil {
		t.Fatal("expected the unrelated route to still be translated")
	}
}

// Composition: a conditions-only policy's codes fold into the SAME merged conditions the
// retry-source policy's own block contributed to, rather than producing a second,
// conflicting RetryPolicy.
func TestResolveRetryDeclarations_SourceAndConditionsCompose(t *testing.T) {
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

	decl, merged, err := tr.resolveRetryDeclarations(chain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decl == nil {
		t.Fatal("expected a retry-source declaration, got nil")
	}
	if len(merged.StatusCodes) != 2 || !containsCode(merged.StatusCodes, 429) || !containsCode(merged.StatusCodes, 401) {
		t.Errorf("merged.StatusCodes = %v, want both 429 (source's own conditions) and 401", merged.StatusCodes)
	}
	if merged.MinAttempts == nil || *merged.MinAttempts != 2 {
		t.Errorf("merged.MinAttempts = %v, want 2 from the conditions-only policy's floor", merged.MinAttempts)
	}
}

// The new capability, end-to-end through createRouteFromRDC: a route carrying ONLY a
// conditions-declaring policy (no retry source, no resilience.retry) still gets a plain,
// non-aggregate RouteAction.RetryPolicy — ordinary same-cluster Envoy retry.
func TestCreateRouteFromRDC_ConditionsOnlyRouteGetsPlainRetryPolicy(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "GET|/api/v1.0/items|"
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
		Upstream:      models.RouteUpstream{ClusterKey: "main"},
	}

	rp := tr.createRouteFromRDC(routeKey, rdcRoute, rdc).GetRoute().GetRetryPolicy()
	if rp == nil {
		t.Fatal("expected a RetryPolicy")
	}
	if rp.RetryPriority != nil {
		t.Error("a conditions-only route must not set RetryPriority (no aggregate cluster exists)")
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

// Replaces the old TestBuildRetryTriggerRetryPolicy_NoCodesYieldsNoPolicy: the
// "contributed nothing, so emit no RetryPolicy at all" rule now lives in
// createRouteFromRDC's guard rather than in a builder's nil return. A policy whose
// conditions resolve to an EMPTY status-code list must leave RetryPolicy unset — an empty
// RetriableStatusCodes list would never fire anyway.
func TestCreateRouteFromRDC_EmptyConditionsYieldNoRetryPolicy(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "GET|/api/v1.0/items|"
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main": {Endpoints: []models.Endpoint{{Host: "echo", Port: 80}}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			routeKey: {Policies: []models.Policy{{
				Name: "trigger-policy", Version: "v1",
				// purge-on-reject explicitly disabled
				Params: map[string]interface{}{"purgeStatusCodes": []interface{}{}},
			}}},
		},
	}
	rdcRoute := &models.Route{
		Method:        "GET",
		Path:          "/api/v1.0/items",
		OperationPath: "/items",
		Upstream:      models.RouteUpstream{ClusterKey: "main"},
	}

	if rp := tr.createRouteFromRDC(routeKey, rdcRoute, rdc).GetRoute().GetRetryPolicy(); rp != nil {
		t.Errorf("expected no RetryPolicy when nothing contributed retry conditions, got %+v", rp)
	}
}

// TestCreateRouteFromRDC_PolicyConditionsComposeWithResilienceRetry is the I1 regression test
// (see the 2026-08-14 final review): a conditions-only policy must UNION its status codes and
// NumRetries floor into an operator's configured resilience.retry, never replace it — unlike a
// retry source, a conditions-only contribution is designed to never conflict with anything.
func TestCreateRouteFromRDC_PolicyConditionsComposeWithResilienceRetry(t *testing.T) {
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
	// NumRetries must stay at the operator's configured 3 — the conditions-only policy's
	// minAttempts-1 (1) is only ever a floor, never a ceiling on the operator's own value.
	if got := rp.GetNumRetries().GetValue(); got != 3 {
		t.Errorf("NumRetries = %d, want 3 (operator's resilience.retry, not clobbered by the policy's conditions)", got)
	}
}

// TestCreateRouteFromRDC_ZeroFallbackSourcePlusConditionsGetsNonZeroNumRetries is the I2
// regression test: a retry-source declaration whose every group is single-target (legal — no
// fallbacks at all, maxChain==0) combined with a conditions-only policy must still get a
// non-zero retry budget, or the merged conditions-only status code can never actually fire.
func TestCreateRouteFromRDC_ZeroFallbackSourcePlusConditionsGetsNonZeroNumRetries(t *testing.T) {
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

// A policy declaring only a minAttempts FLOOR must not collide with an operator's explicit
// numRetries. Merging is not associative over MergeRetryConditions' single-owner rules — it
// derives NumRetries from the floor, so merging an already-merged value back in would
// present that derived count as a second explicit NumRetries owner, fail as a conflict, and
// (because translation swallows the error) silently drop the operator's status codes.
// createRouteFromRDC must therefore feed every contributor into ONE merge pass.
func TestCreateRouteFromRDC_OperatorNumRetriesDoesNotConflictWithPolicyMinAttempts(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "GET|/api/v1.0/items|"
	numRetries := 4
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main": {Endpoints: []models.Endpoint{{Host: "echo", Port: 80}}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			// trigger-policy declares minAttempts: 2 and no numRetries of its own
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
		Timeout:       &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{503}, NumRetries: &numRetries}},
		Upstream:      models.RouteUpstream{ClusterKey: "main"},
	}

	rp := tr.createRouteFromRDC(routeKey, rdcRoute, rdc).GetRoute().GetRetryPolicy()
	if rp == nil {
		t.Fatal("expected a RetryPolicy — a minAttempts floor is not a NumRetries ownership conflict")
	}
	if got := rp.GetNumRetries().GetValue(); got != 4 {
		t.Errorf("NumRetries = %d, want the operator's explicit 4", got)
	}
	if len(rp.RetriableStatusCodes) != 2 {
		t.Errorf("RetriableStatusCodes = %v, want the union of the operator's 503 and the policy's 401", rp.RetriableStatusCodes)
	}
}

// An operator's resilience.retry that omits numRetries must still get the management API's
// documented default of 1 retry — and must express it as a FLOOR, so a policy's own explicit
// numRetries wins rather than colliding with an unrequested exact value.
func TestCreateRouteFromRDC_OperatorRetryWithoutNumRetriesDefaultsToOne(t *testing.T) {
	tr := newRetryTestTranslator()

	routeKey := "GET|/api/v1.0/items|"
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main": {Endpoints: []models.Endpoint{{Host: "echo", Port: 80}}},
		},
	}
	rdcRoute := &models.Route{
		Method:        "GET",
		Path:          "/api/v1.0/items",
		OperationPath: "/items",
		Timeout:       &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{503}}},
		Upstream:      models.RouteUpstream{ClusterKey: "main"},
	}

	rp := tr.createRouteFromRDC(routeKey, rdcRoute, rdc).GetRoute().GetRetryPolicy()
	if rp == nil {
		t.Fatal("expected a RetryPolicy")
	}
	if got := rp.GetNumRetries().GetValue(); got != 1 {
		t.Errorf("NumRetries = %d, want the schema's documented default of 1", got)
	}
}

func TestBuildRoutePolicyFromConditions_NoRetrySource(t *testing.T) {
	two := 2
	merged := policy.RetryConditions{
		On:          []string{"retriable-status-codes"},
		StatusCodes: []int{401},
		MinAttempts: &two,
		NumRetries:  &[]int{1}[0], // MergeRetryConditions derives NumRetries = MinAttempts-1
	}

	rp := buildRoutePolicyFromConditions(merged, nil)

	if rp == nil {
		t.Fatal("expected a non-nil RetryPolicy")
	}
	if rp.GetNumRetries().GetValue() != 1 {
		t.Errorf("expected NumRetries derived from MinAttempts-1 (1), got %d", rp.GetNumRetries().GetValue())
	}
	if len(rp.RetriableStatusCodes) != 1 || rp.RetriableStatusCodes[0] != 401 {
		t.Errorf("unexpected RetriableStatusCodes: %v", rp.RetriableStatusCodes)
	}
	if rp.GetRetryPriority() != nil {
		t.Error("expected no RetryPriority when no retry source is present")
	}
}

func TestBuildRoutePolicyFromConditions_WithRetrySource_SetsRetryPriority(t *testing.T) {
	merged := policy.RetryConditions{StatusCodes: []int{500}}
	source := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "gpt-4o"},
				{UpstreamDefinitionName: "gpt-4o-mini"},
			}},
		},
	}

	rp := buildRoutePolicyFromConditions(merged, source)

	if rp.GetRetryPriority() == nil {
		t.Fatal("expected RetryPriority to be set when a retry source is present")
	}
	if rp.GetRetryPriority().GetName() != "envoy.retry_priorities.previous_priorities" {
		t.Errorf("unexpected RetryPriority name: %q", rp.GetRetryPriority().GetName())
	}
	if rp.GetNumRetries().GetValue() != 1 {
		t.Errorf("expected NumRetries derived from the one-fallback chain length (1), got %d", rp.GetNumRetries().GetValue())
	}
}

func TestBuildRoutePolicyFromConditions_NumRetries_NeverLowerThanChainLength(t *testing.T) {
	// a NumRetries=0 contributor (MinAttempts=1, derived) must never SHRINK the
	// retry-source-derived count
	zero := 0
	one := 1
	merged := policy.RetryConditions{StatusCodes: []int{500}, MinAttempts: &one, NumRetries: &zero}
	source := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "g", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "a"}, {UpstreamDefinitionName: "b"}, {UpstreamDefinitionName: "c"},
			}},
		},
	}

	rp := buildRoutePolicyFromConditions(merged, source)

	if rp.GetNumRetries().GetValue() != 2 {
		t.Errorf("expected NumRetries to stay at the chain length (2), not be lowered by a smaller MinAttempts, got %d", rp.GetNumRetries().GetValue())
	}
}

// buildRoutePolicyFromConditions must emit a DETERMINISTIC status-code list:
// MergeRetryConditions unions codes through a Go map, whose iteration order is
// randomized, and an xDS snapshot that reorders on every rebuild churns the
// resource version for no reason. The old mergeRetriableStatusCodes sorted for
// exactly this reason; the new assembler inherits the obligation.
func TestBuildRoutePolicyFromConditions_EmitsDeterministicSortedOutput(t *testing.T) {
	merged := policy.RetryConditions{
		On:          []string{"retriable-status-codes", "gateway-error", "reset"},
		StatusCodes: []int{503, 401, 502},
	}

	rp := buildRoutePolicyFromConditions(merged, nil)

	want := []uint32{401, 502, 503}
	if len(rp.RetriableStatusCodes) != len(want) {
		t.Fatalf("RetriableStatusCodes = %v, want %v", rp.RetriableStatusCodes, want)
	}
	for i := range want {
		if rp.RetriableStatusCodes[i] != want[i] {
			t.Fatalf("RetriableStatusCodes = %v, want ascending %v", rp.RetriableStatusCodes, want)
		}
	}
	if rp.RetryOn != "gateway-error,reset,retriable-status-codes" {
		t.Errorf("RetryOn = %q, want the sorted union", rp.RetryOn)
	}
}

// PerAttemptTimeout on the retry-source declaration must reach Envoy as
// PerTryTimeout, but it is only ONE MORE CONTRIBUTOR to the single per-try bound
// this route has: the TIGHTEST value across the source and every merged
// contributor wins, and a retry source can never WIDEN a bound another policy in
// the chain already declared. The "contributed is tighter" case below is the one
// that discriminates min-wins from a source-always-wins implementation — with
// only the source-tighter case, both behaviors emit the same value.
func TestBuildRoutePolicyFromConditions_PerTryTimeoutTightestWinsForRetrySource(t *testing.T) {
	dur := func(d time.Duration) *time.Duration { return &d }

	tests := []struct {
		name        string
		perAttempt  *time.Duration // source.PerAttemptTimeout
		contributed *time.Duration // merged.PerTryTimeout
		wantPerTry  *time.Duration // nil == PerTryTimeout must be unset
	}{
		{
			name:        "source tighter than contributed",
			perAttempt:  dur(10 * time.Second),
			contributed: dur(30 * time.Second),
			wantPerTry:  dur(10 * time.Second),
		},
		{
			// The discriminating case: a looser source must NOT discard the
			// tighter contributed bound.
			name:        "contributed tighter than source",
			perAttempt:  dur(30 * time.Second),
			contributed: dur(5 * time.Second),
			wantPerTry:  dur(5 * time.Second),
		},
		{
			name:       "only the source declares one",
			perAttempt: dur(10 * time.Second),
			wantPerTry: dur(10 * time.Second),
		},
		{
			name:        "only a contributor declares one",
			contributed: dur(7 * time.Second),
			wantPerTry:  dur(7 * time.Second),
		},
		{
			name:       "neither declares one leaves it unset",
			wantPerTry: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := policy.RetryConditions{StatusCodes: []int{500}, PerTryTimeout: tt.contributed}
			source := &policy.RetrySourceDeclaration{
				Groups: []policy.RetryGroup{
					// two groups: NumRetries must come from the LONGEST chain, not the first
					{Key: "a", OrderedTargets: []policy.RetryTarget{{}, {UpstreamDefinitionName: "b"}}},
					{Key: "c", OrderedTargets: []policy.RetryTarget{{}, {UpstreamDefinitionName: "d"}, {UpstreamDefinitionName: "e"}}},
				},
				PerAttemptTimeout: tt.perAttempt,
			}

			rp := buildRoutePolicyFromConditions(merged, source)

			if tt.wantPerTry == nil {
				if rp.GetPerTryTimeout() != nil {
					t.Errorf("PerTryTimeout = %v, want unset", rp.GetPerTryTimeout())
				}
			} else if rp.GetPerTryTimeout().AsDuration() != *tt.wantPerTry {
				t.Errorf("PerTryTimeout = %v, want the tightest bound %v", rp.GetPerTryTimeout().AsDuration(), *tt.wantPerTry)
			}
			if rp.GetNumRetries().GetValue() != 2 {
				t.Errorf("NumRetries = %d, want 2 (longest chain 3 targets - 1)", rp.GetNumRetries().GetValue())
			}
		})
	}
}

// A merged contribution's own backOff/avoidPreviousHosts/headers must reach the
// Envoy proto — these are the fields the old status-code-only builders had no
// way to express at all.
func TestBuildRoutePolicyFromConditions_BackOffHeadersAndHostPredicate(t *testing.T) {
	base := 100 * time.Millisecond
	maxInterval := 2 * time.Second
	merged := policy.RetryConditions{
		On:                 []string{"retriable-headers"},
		Headers:            []policy.RetriableHeader{{Name: "x-should-retry", Exact: "true"}},
		BackOff:            &policy.RetryBackOff{BaseInterval: base, MaxInterval: &maxInterval},
		AvoidPreviousHosts: true,
	}

	rp := buildRoutePolicyFromConditions(merged, nil)

	if rp.GetRetryBackOff().GetBaseInterval().AsDuration() != base {
		t.Errorf("BaseInterval = %v, want %v", rp.GetRetryBackOff().GetBaseInterval(), base)
	}
	if rp.GetRetryBackOff().GetMaxInterval().AsDuration() != maxInterval {
		t.Errorf("MaxInterval = %v, want %v", rp.GetRetryBackOff().GetMaxInterval(), maxInterval)
	}
	if len(rp.RetryHostPredicate) != 1 || rp.RetryHostPredicate[0].Name != "envoy.retry_host_predicates.previous_hosts" {
		t.Errorf("RetryHostPredicate = %v, want the previous_hosts predicate", rp.RetryHostPredicate)
	}
	if len(rp.RetriableHeaders) != 1 || rp.RetriableHeaders[0].Name != "x-should-retry" {
		t.Fatalf("RetriableHeaders = %v, want one x-should-retry matcher", rp.RetriableHeaders)
	}
	if got := rp.RetriableHeaders[0].GetStringMatch().GetExact(); got != "true" {
		t.Errorf("RetriableHeaders[0] exact match = %q, want %q", got, "true")
	}
}

// api.Retry's BackOff regenerates as an anonymous nested struct (oapi-codegen
// does not name it RetryBackOff) — this literal's field tags must match
// generated.go's Retry.BackOff exactly for the composite literal's type to
// be identical to the field's type.
func TestOperatorRetryToRawConditions_FullShape(t *testing.T) {
	five := 5
	baseInterval := "100ms"
	avoidHosts := true
	on := []api.RetryOn{"5xx", "connect-failure"}
	r := &api.Retry{
		StatusCodes:        []int{500},
		NumRetries:         &five,
		On:                 &on,
		PerTryTimeout:      strPtr("5s"),
		AvoidPreviousHosts: &avoidHosts,
		BackOff: &struct {
			// BaseInterval Go-duration-formatted base retry backoff interval (e.g. "100ms").
			BaseInterval string `json:"baseInterval" yaml:"baseInterval"`

			// MaxInterval Go-duration-formatted max retry backoff interval.
			MaxInterval *string `json:"maxInterval,omitempty" yaml:"maxInterval,omitempty"`
		}{BaseInterval: baseInterval},
	}

	raw := operatorRetryToRawConditions(r)
	rc, err := config.ParseRetryConditions(raw, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.NumRetries == nil || *rc.NumRetries != 5 {
		t.Errorf("unexpected NumRetries: %v", rc.NumRetries)
	}
	if rc.BackOff == nil || rc.BackOff.BaseInterval != 100*time.Millisecond {
		t.Errorf("unexpected BackOff: %v", rc.BackOff)
	}
	if !rc.AvoidPreviousHosts {
		t.Error("expected AvoidPreviousHosts to survive the round trip")
	}
	if len(rc.On) != 2 || rc.On[0] != "5xx" || rc.On[1] != "connect-failure" {
		t.Errorf("unexpected On: %v", rc.On)
	}
	if rc.PerTryTimeout == nil || *rc.PerTryTimeout != 5*time.Second {
		t.Errorf("unexpected PerTryTimeout: %v", rc.PerTryTimeout)
	}
	if len(rc.StatusCodes) != 1 || rc.StatusCodes[0] != 500 {
		t.Errorf("unexpected StatusCodes: %v", rc.StatusCodes)
	}
}
