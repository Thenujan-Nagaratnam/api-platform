module github.com/wso2/api-platform/dev-policies/body-attempt-echo

go 1.26.5

require github.com/wso2/api-platform/sdk/core v0.2.18

// TEMPORARY, local-validation only - see api-platform PR tracking issue.
// sdk/core v0.2.18 (required above) predates UpstreamAttemptContext.Body /
// UpstreamAttemptRequestModifications.Body and the x-wso2-upstream-attempt
// policy-definition.yaml metadata this dev-only policy exercises - all on
// this checkout's decoupled-retry-source branch but not yet a tagged
// sdk/core release. Relative, not absolute: this copy is nested under
// api-platform (gateway/dev-policies/body-attempt-echo), so ../../../sdk/core
// resolves to api-platform/sdk/core in this checkout - same nesting depth as
// dev-policies/model-failover's replace. Remove this replace and bump the
// require above to a real tagged sdk/core release before merging.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
