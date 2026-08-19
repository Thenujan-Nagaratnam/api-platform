module github.com/wso2/gateway-controllers/policies/model-failover

go 1.26.5

require github.com/wso2/api-platform/sdk/core v0.2.18

// TEMPORARY, local-validation only - see api-platform PR tracking issue.
// sdk/core v0.2.18 (required above) predates policy.RetrySourceUpstreamName,
// policy.UpstreamAttemptPolicy's OnUpstreamAttemptRequest rename, and
// UpstreamAttemptContext.Body/UpstreamAttemptRequestModifications.Body - all
// on api-platform's main checkout (`decoupled-retry-source` branch) but not
// yet a tagged sdk/core release. Relative, not absolute: this copy is nested
// under api-platform (gateway/dev-policies/model-failover), so
// ../../../sdk/core resolves to api-platform/sdk/core in this checkout -
// same nesting depth as dev-policies/advanced-ratelimit's replace. Mirrors
// the same temporary replace in gateway-controllers/policies/model-failover/
// go.mod (that one stays absolute, since gateway-controllers is a separate
// repo with no relative path to sdk/core). Remove this replace and bump the
// require above to a real tagged sdk/core release before merging.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
