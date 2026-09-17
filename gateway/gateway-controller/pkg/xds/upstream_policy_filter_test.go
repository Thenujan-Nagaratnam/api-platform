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

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	upstreamcodecv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/upstream_codec/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
)

func TestAttachUpstreamPolicyFilter_EndsWithTerminalUpstreamCodec(t *testing.T) {
	c := &cluster.Cluster{Name: "test-backend-cluster"}

	err := attachUpstreamPolicyFilter(c, "upstream-policy-engine-cluster")
	require.NoError(t, err)

	require.NotNil(t, c.TypedExtensionProtocolOptions)
	anyOpts, ok := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
	require.True(t, ok, "TypedExtensionProtocolOptions must be keyed by the well-known HttpProtocolOptions type name")

	var opts httpv3.HttpProtocolOptions
	require.NoError(t, anyOpts.UnmarshalTo(&opts))

	require.Len(t, opts.HttpFilters, 2, "the ext_proc filter plus the mandatory terminal upstream_codec filter")
	assert.Equal(t, constants.UpstreamExtProcFilterName, opts.HttpFilters[0].Name)
	assert.Equal(t, constants.UpstreamCodecFilterName, opts.HttpFilters[1].Name,
		"upstream_codec MUST be last or Envoy rejects the cluster at warming time")

	var codecCfg upstreamcodecv3.UpstreamCodec
	require.NoError(t, opts.HttpFilters[1].GetTypedConfig().UnmarshalTo(&codecCfg))
}

func TestAttachUpstreamPolicyFilter_ProducesValidHttpProtocolOptions(t *testing.T) {
	// Envoy rejects the whole CDS update at warming time if
	// HttpProtocolOptions.upstream_protocol_options (a required oneof) is
	// unset — confirmed live: "Proto constraint validation failed (field:
	// \"upstream_protocol_options\", reason: is required)". Catch that class
	// of bug here, in-process, via Envoy's own generated validator, rather
	// than only discovering it against a real Envoy.
	c := &cluster.Cluster{Name: "test-backend-cluster"}

	err := attachUpstreamPolicyFilter(c, "upstream-policy-engine-cluster")
	require.NoError(t, err)

	anyOpts := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
	var opts httpv3.HttpProtocolOptions
	require.NoError(t, anyOpts.UnmarshalTo(&opts))

	assert.NoError(t, opts.ValidateAll())
}

func TestAttachUpstreamPolicyFilter_PointsAtGivenExtProcCluster(t *testing.T) {
	c := &cluster.Cluster{Name: "test-backend-cluster"}

	err := attachUpstreamPolicyFilter(c, "my-upstream-policy-cluster")
	require.NoError(t, err)

	anyOpts := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
	var opts httpv3.HttpProtocolOptions
	require.NoError(t, anyOpts.UnmarshalTo(&opts))

	var extProcCfg extprocv3.ExternalProcessor
	require.NoError(t, opts.HttpFilters[0].GetTypedConfig().UnmarshalTo(&extProcCfg))

	envoyGrpc := extProcCfg.GetGrpcService().GetEnvoyGrpc()
	require.NotNil(t, envoyGrpc)
	assert.Equal(t, "my-upstream-policy-cluster", envoyGrpc.ClusterName)
	// Fail-closed: if the upstream policy engine is unreachable, the request
	// must not proceed unauthenticated/untranslated to the backend.
	assert.False(t, extProcCfg.FailureModeAllow)
	// The server can't know its route/backend from construction (one
	// upstream-policy-engine cluster serves many backend clusters), so it
	// must learn both from Envoy-supplied request attributes.
	assert.Contains(t, extProcCfg.RequestAttributes, constants.ExtProcRequestAttributeRouteName)
	assert.Contains(t, extProcCfg.RequestAttributes, constants.ExtProcRequestAttributeClusterName)
}

func TestAttachUpstreamPolicyFilter_IsIdempotent(t *testing.T) {
	c := &cluster.Cluster{Name: "test-backend-cluster"}

	require.NoError(t, attachUpstreamPolicyFilter(c, "upstream-policy-engine-cluster"))
	require.NoError(t, attachUpstreamPolicyFilter(c, "upstream-policy-engine-cluster"))

	anyOpts := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
	var opts httpv3.HttpProtocolOptions
	require.NoError(t, anyOpts.UnmarshalTo(&opts))

	require.Len(t, opts.HttpFilters, 2, "calling twice must not duplicate the filter chain")
}

func TestShouldAttachUpstreamPolicyFilter(t *testing.T) {
	tests := []struct {
		name                     string
		requiresUpstreamRequest  bool
		requiresUpstreamResponse bool
		want                     bool
	}{
		{"neither", false, false, false},
		{"request only", true, false, true},
		{"response only", false, true, true},
		{"both", true, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAttachUpstreamPolicyFilter(tt.requiresUpstreamRequest, tt.requiresUpstreamResponse)
			assert.Equal(t, tt.want, got)
		})
	}
}
