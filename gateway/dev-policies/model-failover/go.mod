module github.com/wso2/gateway-controllers/policies/model-failover

go 1.26.2

require (
	github.com/stretchr/testify v1.12.1
	github.com/wso2/api-platform/sdk/core v0.3.5
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

// Local dev-policies copy — points at this repo's own sdk/core so the
// unreleased UpstreamRequestContext.RouteCluster field this policy depends
// on is available. See gateway/dev-policies/FAILOVER_TESTING.md. Remove
// this replace once model-failover has a real release with RouteCluster
// available in its published sdk/core dependency.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
