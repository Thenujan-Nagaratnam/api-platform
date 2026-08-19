# Implementation Plan: Option A (Declarative Policy xDS Capabilities)

## 1. Overview & Goal

**Goal:** Completely eliminate all policy-specific code (`if p.Name == "model-failover"`, dedicated parameter structs, hardcoded validators, and duplicated synthetic naming formulas) from the gateway core (`api-platform`), replacing it with a **Declarative Policy xDS Capability Engine**.

**Result:** The gateway compiler/translator becomes 100% policy-agnostic. Any routing, load-balancing, or failover policy (e.g., `model-failover`, `geo-failover`, `canary-router`) can request Envoy aggregate clusters, retry priority configurations, and upstream ext_proc buffering solely by declaring capabilities in its `policy-definition.yaml` manifest.

---

## 2. Architecture & Data Flow

```mermaid
graph TD
    subgraph Policy Definition (gateway-controllers)
        PD[policy-definition.yaml<br/>x-wso2-gateway-capabilities]
        MF[policies/model-failover/model_failover.go]
    end

    subgraph Gateway Control Plane (api-platform)
        PL[PolicyLoader & DefinitionManager]
        VAL[Generic Policy Capability Validator]
        TR[xDS Translator Engine]
        
        PL -->|Parses Capabilities| VAL
        PL -->|Attaches Capabilities to Policy Instances| TR
        TR -->|Generates Aggregate Clusters| AggCluster[envoy.clusters.aggregate]
        TR -->|Generates RetryPolicy| RetryPolicy[RouteAction.RetryPolicy<br/>previous_priorities]
        TR -->|Configures ExtProc Filter| ExtProcFilter[Upstream ext_proc<br/>RequestBodyMode: BUFFERED]
    end

    subgraph Envoy Proxy (Data Plane)
        AggCluster --> Dial[Dial Upstream Targets]
        RetryPolicy --> Failover[Failover on Retriable Status Codes]
    end
```

---

## 3. Specification & Contracts

### 3.1 Declarative Policy Capabilities Schema (`x-wso2-gateway-capabilities`)

In `gateway-controllers/policies/model-failover/policy-definition.yaml`:

```yaml
name: model-failover
version: v1.0.0
displayName: Model Failover
description: Transparent model failover policy for LLM providers

x-wso2-gateway-capabilities:
  # Aggregate cluster generation: instructs translator to synthesize Envoy aggregate clusters
  aggregateClusters:
    enabled: true
    targetListField: "targets"
    groupKeyField: "model"
    fallbackListField: "fallbacks"
    upstreamRefField: "upstreamDefinition"
    syntheticNamePrefix: "__policy_agg__"

  # Retry policy synthesis: instructs translator to attach Envoy RetryPolicy to route
  retryPolicy:
    type: "previous-priorities"
    statusCodesField: "statusCodes"
    timeoutField: "requestTimeout"
    retryCountFrom: "max-fallback-depth"
    updateFrequency: 1

  # Upstream ext_proc buffering: enables per-attempt request body mutation
  extProc:
    upstreamAttempt:
      requestBodyMode: "BUFFERED"

parameters:
  type: object
  required: [targets, statusCodes]
  properties:
    targets:
      type: array
      items:
        type: object
        required: [model]
        properties:
          model:
            type: string
          upstreamDefinition:
            type: string
            x-wso2-upstream-reference: true
          fallbacks:
            type: array
            items:
              type: object
              required: [model]
              properties:
                model:
                  type: string
                upstreamDefinition:
                  type: string
                  x-wso2-upstream-reference: true
    statusCodes:
      type: array
      items:
        type: integer
    requestTimeout:
      type: string
    suspendDuration:
      type: string
```

### 3.2 Standardized Synthetic Cluster Naming Convention

Exported in `github.com/wso2/api-platform/sdk/core/policy/v1alpha2`:

```go
// PolicyAggregateClusterName produces the standardized aggregate cluster upstream name.
// Formula: __policy_agg__<SafeRouteKey>__<GroupKey>
func PolicyAggregateClusterName(routeName, groupKey string) string {
    safeRoute := strings.ReplaceAll(routeName, "|", "_")
    if routeName == "" {
        return "__policy_agg__" + groupKey
    }
    return "__policy_agg__" + safeRoute + "__" + groupKey
}
```

---

## 4. Implementation Tasks

### Phase 1: Models & Generic Capability Parser (`api-platform`)

- [ ] **Task 1.1: Add Capability Structs to Models**
  - **File**: `gateway/gateway-controller/pkg/models/policy_capabilities.go`
  - Define `PolicyCapabilities`, `AggregateClustersCapability`, `RetryPolicyCapability`, `ExtProcCapability`.
  - Add `Capabilities *PolicyCapabilities` to `models.PolicyDefinition` and `models.Policy`.

- [ ] **Task 1.2: Generic Upstream Reference & Collision Validator**
  - **File**: `gateway/gateway-controller/pkg/config/generic_policy_validator.go`
  - Implement `ValidatePolicyCapabilitiesForOperations(spec *api.APIConfigData, policyDefs map[string]models.PolicyDefinition) error`.
  - Check for:
    1. Collision between policy `retryPolicy` and route-level `resilience.retry`.
    2. Missing declared `spec.upstreamDefinitions` referenced by properties with `x-wso2-upstream-reference: true`.
    3. Aggregate cluster member `BasePath` non-empty limitation.

- [ ] **Task 1.3: Delete Policy-Specific Validator**
  - Delete `gateway/gateway-controller/pkg/config/model_failover_validator.go`.
  - Replace specific validation calls in `pkg/utils/llm_deployment.go` and `pkg/utils/api_deployment.go` with `ValidatePolicyCapabilitiesForOperations`.

---

### Phase 2: Generic xDS Translator Engine (`api-platform`)

- [ ] **Task 2.1: Generic Aggregate Cluster Generation**
  - **File**: `gateway/gateway-controller/pkg/xds/translator.go`
  - In `translateRuntimeConfig`, replace `p.Name == "model-failover"` with generic capability extraction.

- [ ] **Task 2.2: Generic Retry Policy Generation**
  - **File**: `gateway/gateway-controller/pkg/xds/translator.go`
  - In `createRouteFromRDC`, replace `p.Name == "model-failover"` with generic `buildGenericRetryPolicy`.

- [ ] **Task 2.3: Generic ExtProc Upstream Buffer Attachment**
  - **File**: `gateway/gateway-controller/pkg/xds/translator.go`
  - Replace `isModelFailoverAggregateCluster` with `isAggregateCluster(c *cluster.Cluster)` to automatically mark all generated aggregate clusters for upstream ext_proc buffering.

---

### Phase 3: Update Policy & Policy Manifest (`gateway-controllers`)

- [ ] **Task 3.1: Update `policy-definition.yaml`**
  - **File**: `gateway-controllers/policies/model-failover/policy-definition.yaml`
  - Add `x-wso2-gateway-capabilities` block and `x-wso2-upstream-reference: true` annotations.

- [ ] **Task 3.2: Update Policy Implementation**
  - **File**: `gateway-controllers/policies/model-failover/model_failover.go`
  - Use `PolicyAggregateClusterName` from SDK.

---

## 5. Verification Plan

1. **Unit Tests**:
   - `cd gateway/gateway-controller && go test ./pkg/config/... ./pkg/xds/... -v`
   - `cd gateway-controllers/policies/model-failover && go test ./... -v`
2. **E2E Postman Suite**:
   - Run `gateway-controllers/policies/model-failover/e2e/run-e2e.sh` to verify zero regression across all failover scenarios.
