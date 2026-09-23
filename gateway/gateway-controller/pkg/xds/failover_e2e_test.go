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
	"strings"
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	compositev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/composite/v3"
	resource "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// This file's assertions come in two halves, deliberately split across
// packages (documented as an approved deviation from the task brief, which
// named a single file/chain): pkg/transform/llm_failover_e2e_test.go runs
// the REAL LLMTransformer.Transform chain (DB-scaffolded provider/template
// resolution) and asserts on the resulting RuntimeDeployConfig's resolved
// Failover ClusterKeys — that is what would have caught a schema-scoping
// mistake like sharing LLMResilience with RestApi. This file, staying inside
// package xds, asserts the xDS-translation half (aggregate clusters, retry
// policy, host rewrite, VirtualHost.IncludeRequestAttemptCount) against
// hand-built RuntimeDeployConfig fixtures.
//
// Why hand-built fixtures instead of calling transform.LLMTransformer here
// too: pkg/transform (and pkg/utils, which it depends on for LLM transforms)
// already import pkg/xds in production code (pkg/transform/restapi.go,
// pkg/utils/*_deployment.go), so a package-xds test file importing
// pkg/transform is a real import cycle — verified via `go vet` returning
// "import cycle not allowed in test". translateRuntimeConfig is also
// unexported, so an external xds_test package (which could import
// pkg/transform) can't reach it either. Exporting a test-only wrapper was
// considered and rejected: it would be new production surface for no real
// benefit when the existing models.ConfigTransformer / SetTransformers
// extension point (translator.go's TranslateConfigs already decouples
// itself from pkg/transform this way in production) does the job.

// fakeFailoverTransformer implements models.ConfigTransformer to hand a
// pre-built RuntimeDeployConfig straight to the real TranslateConfigs
// pipeline, so these tests exercise genuine vhost-assembly production code
// (including the IncludeRequestAttemptCount computation at translator.go's
// vhostNeedsAttemptCount) without needing pkg/transform's real transform chain.
type fakeFailoverTransformer struct {
	rdc *models.RuntimeDeployConfig
}

func (f *fakeFailoverTransformer) Transform(_ *models.StoredConfig) (*models.RuntimeDeployConfig, error) {
	return f.rdc, nil
}

const failoverFixtureKind = "FailoverE2EFixture"

// translateFixture runs rdc through the real TranslateConfigs entry point
// (via the fake transformer above) and returns the "main" virtual host plus
// the flat cluster list, for assertions.
func translateFixture(t *testing.T, rdc *models.RuntimeDeployConfig) (*route.VirtualHost, []*cluster.Cluster) {
	t.Helper()

	translator := createTestTranslator()
	translator.SetTransformers(map[string]models.ConfigTransformer{
		failoverFixtureKind: &fakeFailoverTransformer{rdc: rdc},
	})

	cfg := &models.StoredConfig{
		UUID:         "failover-e2e-fixture",
		Kind:         failoverFixtureKind,
		DisplayName:  "failover-e2e-fixture",
		DesiredState: models.StateDeployed,
	}

	resources, err := translator.TranslateConfigs([]*models.StoredConfig{cfg}, "failover-e2e-test")
	require.NoError(t, err)

	var vh *route.VirtualHost
	for _, res := range resources[resource.RouteType] {
		rc, ok := res.(*route.RouteConfiguration)
		require.True(t, ok, "route resource must be a *route.RouteConfiguration")
		for _, candidate := range rc.VirtualHosts {
			if candidate.Name == "main" {
				vh = candidate
			}
		}
	}
	require.NotNil(t, vh, `expected a "main" virtual host in the translated route config`)

	clusters := make([]*cluster.Cluster, 0, len(resources[resource.ClusterType]))
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok, "cluster resource must be a *cluster.Cluster")
		clusters = append(clusters, c)
	}
	return vh, clusters
}

func findFixtureRoute(t *testing.T, vh *route.VirtualHost, name string) *route.Route {
	t.Helper()
	for _, r := range vh.Routes {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("route %q not found in virtual host %q", name, vh.Name)
	return nil
}

// compositeClustersIn filters clusters down to the retry-aware model-failover
// chain clusters —
// the translated cluster list otherwise also contains the policy engine,
// upstream policy engine, and any ALS/OTEL clusters TranslateConfigs always adds.
// compositeClustersIn returns only the FULL-CHAIN composite clusters (one per
// targets[] entry, named via AggregateClusterName) — NOT the suffix
// composites buildFailoverCompositeClusters also generates per fallback
// position (SuffixCompositeClusterName, "failover_suffix_..."). Both share
// discovery type "envoy.clusters.composite", so filtering by name prefix is
// what distinguishes them; see suffixCompositeClustersIn for the other set.
func compositeClustersIn(clusters []*cluster.Cluster) []*cluster.Cluster {
	var composites []*cluster.Cluster
	for _, c := range clusters {
		if c.GetClusterType().GetName() == "envoy.clusters.composite" && strings.HasPrefix(c.Name, "failover_composite_") {
			composites = append(composites, c)
		}
	}
	return composites
}

// suffixCompositeClustersIn returns only the suffix composites — see
// compositeClustersIn's doc comment for the distinction.
func suffixCompositeClustersIn(clusters []*cluster.Cluster) []*cluster.Cluster {
	var composites []*cluster.Cluster
	for _, c := range clusters {
		if c.GetClusterType().GetName() == "envoy.clusters.composite" && strings.HasPrefix(c.Name, "failover_suffix_") {
			composites = append(composites, c)
		}
	}
	return composites
}

func compositeMembersOf(t *testing.T, c *cluster.Cluster) []string {
	t.Helper()
	var cfg compositev3.ClusterConfig
	require.NoError(t, c.GetClusterType().GetTypedConfig().UnmarshalTo(&cfg))
	members := make([]string, 0, len(cfg.Clusters))
	for _, entry := range cfg.Clusters {
		members = append(members, entry.Name)
	}
	return members
}

// findClusterByName returns the cluster named name from the full translated
// cluster list, or nil — used to look up a model-failover leaf cluster
// (built by buildFailoverCompositeClusters, cloned from its source and
// appended alongside the composite, never itself of discovery type
// "envoy.clusters.composite") among everything TranslateConfigs produced.
func findClusterByName(clusters []*cluster.Cluster, name string) *cluster.Cluster {
	for _, c := range clusters {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestLLMTransform_NoFailoverBlock_OutputUnchangedFromBeforeThisFeature is the
// concrete proof of this plan's Global Constraint: a route with no
// model-failover attachment produces byte-identical xDS output to before
// this feature existed — no aggregate cluster, no retry policy, no
// failover-driven host rewrite, and the containing virtual host does not
// request the attempt-count header. This is the same fixture Task 5's test
// (TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite,
// upstream_policy_filter_wiring_test.go) uses, with Upstream.Failover left
// nil entirely (the pre-this-feature shape).
func TestLLMTransform_NoFailoverBlock_OutputUnchangedFromBeforeThisFeature(t *testing.T) {
	routeKey := "POST|/chat/completions|main"
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
		},
		Routes: map[string]*models.Route{
			routeKey: {
				Method: "POST",
				Path:   "/chat/completions",
				Vhost:  "main",
				Upstream: models.RouteUpstream{
					ClusterKey:       "primary-cluster",
					UseClusterHeader: true,
					DefaultCluster:   "primary-cluster",
					// Failover intentionally left nil.
				},
			},
		},
	}

	vh, clusters := translateFixture(t, rdc)

	assert.Empty(t, compositeClustersIn(clusters),
		"no composite cluster should be created for a route with no failover block")

	r := findFixtureRoute(t, vh, routeKey)
	action := r.GetRoute()
	require.NotNil(t, action)
	assert.Nil(t, action.RetryPolicy, "a non-failover route must carry no RetryPolicy")

	_, isAutoRewrite := action.HostRewriteSpecifier.(*route.RouteAction_AutoHostRewrite)
	assert.False(t, isAutoRewrite,
		"HostRewriteSpecifier must reflect only the pre-existing AutoHostRewrite bool field, never failover-driven rewrite")

	assert.False(t, vh.IncludeRequestAttemptCount,
		"a virtual host with no failover route must not request the attempt-count header")
}

// TestLLMTransform_FailoverBlock_FullShapeEndToEnd asserts the full xDS shape
// for a route with one model-failover targets[] entry: primary provider
// "openai-provider" attempting model "gpt-4o", falling back to
// "claude-sonnet-4-5-20250929" on additionalProvider "anthropic-provider"
// (as: "anthropic-upstream"). The provider/template resolution half of this
// scenario (that "anthropic-upstream" correctly resolves to the
// additionalProvider's cluster, in order) is proven against the REAL
// LLMTransformer.Transform chain in
// pkg/transform/llm_failover_e2e_test.go — this test proves what
// TranslateConfigs does with that already-resolved shape.
func TestLLMTransform_FailoverBlock_FullShapeEndToEnd(t *testing.T) {
	routeKey := "POST|/chat/completions|main"
	openaiClusterKey := "openai-provider-cluster"
	anthropicClusterKey := "anthropic-upstream-cluster"

	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			openaiClusterKey:    {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			anthropicClusterKey: {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
		},
		Routes: map[string]*models.Route{
			routeKey: {
				Method: "POST",
				Path:   "/chat/completions",
				Vhost:  "main",
				Upstream: models.RouteUpstream{
					ClusterKey:       openaiClusterKey,
					UseClusterHeader: true,
					DefaultCluster:   openaiClusterKey,
					Failover: &models.RouteFailover{
						Targets: []models.RouteFailoverTarget{{
							Model: "gpt-4o",
							Target: models.RouteFailoverEntry{
								Model:      "gpt-4o",
								ClusterKey: openaiClusterKey,
							},
							Fallbacks: []models.RouteFailoverEntry{{
								Model:      "claude-sonnet-4-5-20250929",
								ClusterKey: anthropicClusterKey,
							}},
						}},
					},
				},
			},
		},
	}

	vh, clusters := translateFixture(t, rdc)

	composites := compositeClustersIn(clusters)
	require.Len(t, composites, 1, "exactly one composite cluster should exist for one failover targets[] entry")
	assert.Equal(t, AggregateClusterName(routeKey, 0), composites[0].Name)
	assert.Equal(t, []string{FailoverLeafClusterName(routeKey, 0, 0), FailoverLeafClusterName(routeKey, 0, 1)}, compositeMembersOf(t, composites[0]),
		"composite members must be isolated leaves in target/fallback order")

	// Every composite member must also be a real leaf cluster in the full
	// translated cluster list (not merely a name inside the composite's own
	// config), and must carry NO outlier_detection/panic override — live
	// verification against a real Envoy showed leaf ejection breaks failover
	// instead of helping it (see configureFailoverLeafCircuitBreaker's doc
	// comment). Cross-request suspension of a known-bad primary is
	// policy-side again (model-failover's isSuspended), not Envoy-native.
	for i, leafName := range []string{FailoverLeafClusterName(routeKey, 0, 0), FailoverLeafClusterName(routeKey, 0, 1)} {
		leaf := findClusterByName(clusters, leafName)
		require.NotNilf(t, leaf, "leaf cluster %q (position %d) must be present in the translated cluster list", leafName, i)
		assert.Nil(t, leaf.OutlierDetection)
		assert.Nil(t, leaf.CommonLbConfig)
	}

	r := findFixtureRoute(t, vh, routeKey)
	action := r.GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	assert.Equal(t, "5xx", action.RetryPolicy.RetryOn)

	_, isAutoRewrite := action.HostRewriteSpecifier.(*route.RouteAction_AutoHostRewrite)
	assert.True(t, isAutoRewrite, "a failover route must auto-rewrite Host")

	assert.False(t, vh.IncludeRequestAttemptCount,
		"isolated leaf identity removes failover's dependency on attempt-count inference")
}

// TestLLMTransform_MultipleFailoverTargets_EachEntryIsolated proves that two
// independent targets[] entries — each with its own fallback chain of a
// different length — get their own composite cluster and their own,
// non-colliding leaf clusters, and that the route's single RetryPolicy
// (NumRetries) reflects the DEEPEST entry rather than either one alone.
func TestLLMTransform_MultipleFailoverTargets_EachEntryIsolated(t *testing.T) {
	routeKey := "POST|/chat/completions|main"
	openaiClusterKey := "openai-provider-cluster"
	anthropicClusterKey := "anthropic-upstream-cluster"
	cohereClusterKey := "cohere-upstream-cluster"

	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			openaiClusterKey:    {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			anthropicClusterKey: {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			cohereClusterKey:    {BasePath: "/", Endpoints: []models.Endpoint{{Host: "cohere.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
		},
		Routes: map[string]*models.Route{
			routeKey: {
				Method: "POST",
				Path:   "/chat/completions",
				Vhost:  "main",
				Upstream: models.RouteUpstream{
					ClusterKey:       openaiClusterKey,
					UseClusterHeader: true,
					DefaultCluster:   openaiClusterKey,
					Failover: &models.RouteFailover{
						Targets: []models.RouteFailoverTarget{
							{
								Model:  "gpt-4o",
								Target: models.RouteFailoverEntry{Model: "gpt-4o", ClusterKey: openaiClusterKey},
								Fallbacks: []models.RouteFailoverEntry{
									{Model: "claude-sonnet-4-5-20250929", ClusterKey: anthropicClusterKey},
								},
							},
							{
								Model:  "gpt-4o-mini",
								Target: models.RouteFailoverEntry{Model: "gpt-4o-mini", ClusterKey: openaiClusterKey},
								Fallbacks: []models.RouteFailoverEntry{
									{Model: "claude-sonnet-4-5-20250929", ClusterKey: anthropicClusterKey},
									{Model: "command-r-plus", ClusterKey: cohereClusterKey},
								},
							},
						},
					},
				},
			},
		},
	}

	vh, clusters := translateFixture(t, rdc)

	composites := compositeClustersIn(clusters)
	require.Len(t, composites, 2, "each targets[] entry must get its own composite cluster")

	composite0 := findClusterByName(clusters, AggregateClusterName(routeKey, 0))
	require.NotNil(t, composite0)
	assert.Equal(t,
		[]string{FailoverLeafClusterName(routeKey, 0, 0), FailoverLeafClusterName(routeKey, 0, 1)},
		compositeMembersOf(t, composite0), "entry 0 (1 fallback) members in target/fallback order")

	composite1 := findClusterByName(clusters, AggregateClusterName(routeKey, 1))
	require.NotNil(t, composite1)
	assert.Equal(t,
		[]string{FailoverLeafClusterName(routeKey, 1, 0), FailoverLeafClusterName(routeKey, 1, 1), FailoverLeafClusterName(routeKey, 1, 2)},
		compositeMembersOf(t, composite1), "entry 1 (2 fallbacks) members in target/fallback order")

	seen := map[string]bool{}
	for _, name := range append(compositeMembersOf(t, composite0), compositeMembersOf(t, composite1)...) {
		assert.Falsef(t, seen[name], "leaf cluster name %q must not repeat across independent targets[] entries", name)
		seen[name] = true
		assert.NotNilf(t, findClusterByName(clusters, name), "leaf %q must exist in the translated cluster list", name)
	}

	r := findFixtureRoute(t, vh, routeKey)
	action := r.GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	require.NotNil(t, action.RetryPolicy.NumRetries)
	assert.Equal(t, uint32(2), action.RetryPolicy.NumRetries.GetValue(),
		"one route-level RetryPolicy shared by both entries must use the deeper entry's chain length")
}

// TestLLMTransform_FailoverBlock_ConfiguredStatusCodesWireIntoRetryPolicy
// pins that a non-empty RouteFailover.RetriableStatusCodes switches RetryOn
// to "retriable-status-codes" (not "5xx" alongside it — statusCodes REPLACES
// the default) and carries the exact codes onto Envoy's own RetryPolicy.
func TestLLMTransform_FailoverBlock_ConfiguredStatusCodesWireIntoRetryPolicy(t *testing.T) {
	routeKey := "POST|/chat/completions|main"
	openaiClusterKey := "openai-provider-cluster"
	anthropicClusterKey := "anthropic-upstream-cluster"

	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			openaiClusterKey:    {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			anthropicClusterKey: {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.example.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
		},
		Routes: map[string]*models.Route{
			routeKey: {
				Method: "POST",
				Path:   "/chat/completions",
				Vhost:  "main",
				Upstream: models.RouteUpstream{
					ClusterKey:       openaiClusterKey,
					UseClusterHeader: true,
					DefaultCluster:   openaiClusterKey,
					Failover: &models.RouteFailover{
						RetryOn:              []string{"retriable-status-codes"},
						RetriableStatusCodes: []int{500, 502, 429},
						Targets: []models.RouteFailoverTarget{{
							Model: "gpt-4o",
							Target: models.RouteFailoverEntry{
								Model:      "gpt-4o",
								ClusterKey: openaiClusterKey,
							},
							Fallbacks: []models.RouteFailoverEntry{{
								Model:      "claude-sonnet-4-5-20250929",
								ClusterKey: anthropicClusterKey,
							}},
						}},
					},
				},
			},
		},
	}

	vh, clusters := translateFixture(t, rdc)

	r := findFixtureRoute(t, vh, routeKey)
	action := r.GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	assert.Equal(t, "retriable-status-codes", action.RetryPolicy.RetryOn, "must not also carry plain 5xx")
	assert.Equal(t, []uint32{500, 502, 429}, action.RetryPolicy.RetriableStatusCodes)

	// Leaves themselves carry no outlier_detection at all now (see
	// configureFailoverLeafCircuitBreaker's doc comment), so RetriableStatusCodes
	// only has one consumer: the route's own RetryPolicy asserted above.
	for _, leafName := range []string{FailoverLeafClusterName(routeKey, 0, 0), FailoverLeafClusterName(routeKey, 0, 1)} {
		leaf := findClusterByName(clusters, leafName)
		require.NotNilf(t, leaf, "leaf cluster %q must be present in the translated cluster list", leafName)
		assert.Nil(t, leaf.OutlierDetection)
	}
}
