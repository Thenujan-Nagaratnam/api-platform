module github.com/wso2/gateway-controllers/policies/model-failover

go 1.26.5

require github.com/wso2/api-platform/sdk/core v0.2.18

// POC-local only. This policy now only uses sdk/core symbols already present in v0.2.18
// (the response-path retry mechanism deliberately avoids the alpha upstream-ext_proc
// types, see model_failover.go's package doc) - this replace exists solely so the POC
// builds against this exact checkout's sdk/core without depending on module-proxy
// availability for v0.2.18 specifically. Relative, not absolute: this copy is nested
// under api-platform (gateway/dev-policies/model-failover), so ../../../sdk/core
// resolves to api-platform/sdk/core in this checkout. Remove this replace once a
// tagged sdk/core release is confirmed sufficient and reachable in CI.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
