module github.com/wso2/api-platform/gateway/sample-policies/retry-on-status

go 1.26.5

require github.com/wso2/api-platform/sdk/core v0.4.1

// sdk/core/attempts is not in a released sdk/core yet; see the policy engine's go.mod.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
