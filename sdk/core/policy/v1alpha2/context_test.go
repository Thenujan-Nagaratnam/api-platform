/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package policyv1alpha2

import "testing"

func TestUpstreamRequestContext_RouteClusterField(t *testing.T) {
	ctx := &UpstreamRequestContext{Name: "resolved-real-cluster", RouteCluster: "failover_agg_abc_0"}
	if ctx.RouteCluster != "failover_agg_abc_0" {
		t.Fatalf("RouteCluster = %q, want %q", ctx.RouteCluster, "failover_agg_abc_0")
	}
	if ctx.Name != "resolved-real-cluster" {
		t.Fatalf("Name = %q, want %q, RouteCluster must not alias Name", ctx.Name, "resolved-real-cluster")
	}
}

func TestUpstreamResponseContext_RouteClusterField(t *testing.T) {
	ctx := &UpstreamResponseContext{Name: "resolved-real-cluster", RouteCluster: "failover_agg_abc_0"}
	if ctx.RouteCluster != "failover_agg_abc_0" {
		t.Fatalf("RouteCluster = %q, want %q", ctx.RouteCluster, "failover_agg_abc_0")
	}
}
