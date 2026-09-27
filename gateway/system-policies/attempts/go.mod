module github.com/wso2/api-platform/gateway/system-policies/attempts

go 1.26.5

// Pinned to the version the policy engine itself requires, never ahead of it.
require github.com/wso2/api-platform/sdk/core v0.4.1

// The attempts package is not in a released sdk/core yet. The policy engine
// builds against the same local copy (see its go.mod); both replaces go when
// sdk/core is tagged with it.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
