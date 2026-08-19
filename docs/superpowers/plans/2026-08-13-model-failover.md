# AI Gateway Model Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new `model-failover` policy that, given an ordered list of model+`upstreamDefinition` targets, transparently retries a failed request against the next target (a genuinely different upstream, not just a different endpoint of the same one) — rewriting the request body's `model` field per attempt — so the client only ever sees the final outcome. A model-endpoint pair that just failed is suspended for a configurable duration so the *next* request skips straight past it.

**Architecture:** Envoy `aggregate cluster` (`envoy.clusters.aggregate`) composes the already-existing per-`upstreamDefinition` clusters in priority order; `RouteAction.RetryPolicy` with `retry_priority: previous_priorities` fails over between them within one client request. The existing retry-refresh `UpstreamAttemptPolicy` mechanism is extended (additively) with body support so the policy can rewrite `model` on each attempt. Suspend state uses the next request's *normal* (non-retry) request phase, redirecting via the existing `UpstreamName` → `upstreamDefinitions` routing path — no new routing mechanism, no Envoy health-state manipulation.

**Tech Stack:** Go 1.23+, `google.golang.org/grpc`, `github.com/envoyproxy/go-control-plane/envoy` v1.37.0 (`extensions/clusters/aggregate/v3`, `extensions/retry/priority/previous_priorities/v3`), stock `envoyproxy/envoy:v1.38.3` image, `github.com/redis/go-redis/v9` (via `sdk/core/utils/redisclient`).

**Builds on:** `docs/superpowers/specs/2026-08-13-model-failover-design.md` (read this first — it has the two verified Envoy spikes this plan's Phase 3 depends on). Reuses the `UpstreamAttemptPolicy` mechanism from `docs/superpowers/specs/2026-08-12-upstream-attempt-retry-refresh-design.md`.

## Global Constraints

- No custom-compiled Envoy — must work against the stock `envoyproxy/envoy:${ENVOY_VERSION}` image already in use (currently `v1.38.3`).
- `model-failover` and `resilience.retry` are mutually exclusive on the same route for v1 — reject at validation if both are configured. They'd otherwise fight over `RouteAction.RetryPolicy`.
- Ext_proc request-body buffering (`RequestBodyMode: BUFFERED`) is opt-in per-cluster, attached only to `model-failover`'s aggregate clusters — never widened onto plain `resilience.retry`/oauth2-generator clusters, which stay headers-only. This is a *separate* capability flag from the existing `clustersNeedingUpstreamFilter`, not a widening of it.
- The upstream ext_proc filter enabling body mutation for a `model-failover` route must be attached to the **aggregate cluster itself**, never its member clusters — attaching to members is a silent no-op (verified in the design spec's spike). Get this wrong and the feature silently does nothing.
- Fail open everywhere in the new policy: cache unavailable, malformed config, missing `upstreamDefinition` reference → proceed as if `model-failover` weren't configured for that attempt, never a new way to fail a request.
- `gateway-controllers/policies/model-failover` (source of truth, a separate repo) is authored **first**; `dev-policies/model-failover` (this repo) is a byte-identical mirror copied from it — the reverse order of how `oauth2-generator`'s retry-refresh work was done. Diff after copying, every time.
- No raw upstream response bodies/tokens in log output — this policy only ever logs model names, target indices, and status codes, never body content.

---

## Phase 0: Verify the prerequisite this whole design depends on

### Task 1: Verify `upstreamDefinitions` produces a real, named cluster for `kind: LlmProvider` on the live path

**Files:**
- None modified — this is a verification-only task using existing infrastructure.

**Interfaces:**
- Consumes: `gateway-controller`'s `POST /api/management/v1/llm-providers` endpoint; `gateway-runtime`'s Envoy admin `/config_dump` (port 9901).
- Produces: a confirmed fact (or, if it fails, a documented gap) that Task 5 depends on: `LLMProviderConfigData.UpstreamDefinitions` (confirmed present at `gateway-controller/pkg/api/management/generated.go:757-758`) flows through `LLMProviderTransformer.transformProvider` (`pkg/utils/llm_transformer.go:491`, `spec.UpstreamDefinitions = provider.Spec.UpstreamDefinitions`) into `RestAPITransformer`'s already-confirmed-working cluster-creation loop (`pkg/transform/restapi.go:240-284`), producing a cluster named `upstream_LlmProvider_<uuid>_<sanitizedName>`.

- [ ] **Step 1: Bring up the stack if not already running**

```bash
cd gateway
docker compose up -d gateway-controller gateway-runtime sample-backend
sleep 3
curl -sk -o /dev/null -w "controller: %{http_code}\n" -u admin:admin http://localhost:9090/api/management/v1/rest-apis
curl -sk -o /dev/null -w "runtime: %{http_code}\n" https://localhost:8443/
```
Expected: `controller: 200`, `runtime: 404` (404 at `/` is normal — no route matches root).

- [ ] **Step 2: Register an LlmProvider with `upstreamDefinitions`**

```bash
curl -sk -X POST http://localhost:9090/api/management/v1/llm-providers \
  -u admin:admin -H "Content-Type: application/yaml" \
  --data-binary $'apiVersion: gateway.api-platform.wso2.com/v1\nkind: LlmProvider\nmetadata:\n  name: verify-upstream-definitions\nspec:\n  displayName: Verify upstreamDefinitions\n  version: v1.0\n  template: openai\n  context: /verify-upstream-definitions/latest\n  upstream:\n    url: http://host.docker.internal:9701\n  upstreamDefinitions:\n    - name: side-channel\n      upstreams:\n        - url: http://host.docker.internal:9702\n  accessControl:\n    mode: deny_all\n    exceptions:\n      - path: /chat/completions\n        methods: [POST]\n' \
  -w "\nHTTP: %{http_code}\n"
```
Expected: `HTTP: 201`. Note the response's `status.id` (a UUID) — you'll need it for Step 3.

- [ ] **Step 3: Check Envoy's live config for the expected cluster name**

```bash
sleep 2
curl -s http://localhost:19901/config_dump 2>/dev/null | grep -o "upstream_LlmProvider_[a-zA-Z0-9-]*_side_channel" | sort -u
```
(If `19901` isn't right for this stack's admin port, use whatever `RUNTIME_ADMIN_URL`/`docker-compose.yaml` actually maps — check with `docker port` on the `gateway-runtime` container if unsure.)

Expected: one line printed, matching `upstream_LlmProvider_<the-uuid-from-step-2>_side_channel`.

- [ ] **Step 4: Record the outcome**

If Step 3 printed the expected cluster name: the prerequisite holds — proceed to Phase 1 as written.

If Step 3 printed nothing: the cluster isn't being created on the live path despite `transformProvider` carrying the field through. Before proceeding, add this instrumentation to isolate where it's dropped, then fix forward:

```bash
cd gateway-controller
grep -n "UpstreamDefinitions" pkg/transform/restapi.go
```
Confirm `RestAPITransformer.Transform` (or whichever method builds `rdc.UpstreamClusters`) is actually being invoked for `kind: LlmProvider` requests — check `cmd/controller/main.go`'s transformer registration (`SetTransformers`) to see which kind string routes to which transformer, and whether `LLMProviderTransformer`'s output (`*api.RestAPI`) is handed to `RestAPITransformer` afterward or to some other path. Fix whatever hop is missing, re-run Steps 2-3, and update this task's outcome before continuing.

- [ ] **Step 5: Clean up**

```bash
curl -sk -o /dev/null -w "%{http_code}\n" -X DELETE http://localhost:9090/api/management/v1/llm-providers/verify-upstream-definitions -u admin:admin
```

---

## Phase 1: SDK — additive body support on the upstream-attempt phase

### Task 2: Add `Body` to `UpstreamAttemptContext` and `UpstreamAttemptHeaderModifications`

**Files:**
- Modify: `sdk/core/policy/v1alpha2/context.go` (the `UpstreamAttemptContext` struct, currently lines 319-331)
- Modify: `sdk/core/policy/v1alpha2/action.go` (the `UpstreamAttemptHeaderModifications` struct, currently lines 304-312)
- Test: `sdk/core/policy/v1alpha2/upstream_attempt_test.go`

**Interfaces:**
- Consumes: nothing new — purely additive fields on existing exported types.
- Produces: `UpstreamAttemptContext.Body *Body` (nil when the cluster's ext_proc filter isn't body-buffered); `UpstreamAttemptHeaderModifications.Body []byte` (nil = no change, matching every other `Body []byte` field in this package, e.g. `UpstreamRequestModifications.Body`).

- [ ] **Step 1: Write the failing test**

```go
// added to upstream_attempt_test.go
func TestUpstreamAttemptContext_BodyFieldExists(t *testing.T) {
	actx := &UpstreamAttemptContext{
		AttemptCount: 2,
		Headers:      NewHeaders(nil),
		Body:         &Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true, EndOfStream: true},
	}
	if actx.Body == nil || string(actx.Body.Content) != `{"model":"gpt-4o"}` {
		t.Fatalf("expected Body.Content to round-trip, got %#v", actx.Body)
	}
}

func TestUpstreamAttemptHeaderModifications_BodyFieldExists(t *testing.T) {
	mods := UpstreamAttemptHeaderModifications{
		HeadersToSet: map[string]string{"Authorization": "Bearer x"},
		Body:         []byte(`{"model":"gpt-4o-mini"}`),
	}
	if string(mods.Body) != `{"model":"gpt-4o-mini"}` {
		t.Fatalf("expected Body to round-trip, got %q", mods.Body)
	}
	var _ UpstreamAttemptAction = mods // still satisfies the sealed interface
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestUpstreamAttemptContext_BodyFieldExists -v`
Expected: FAIL with `unknown field 'Body' in struct literal`.

- [ ] **Step 3: Add the fields**

In `context.go`, extend the existing struct (do not add a new type):

```go
type UpstreamAttemptContext struct {
	*SharedContext

	// AttemptCount is Envoy's x-envoy-attempt-count for this specific dial,
	// starting at 1. A missing/unparseable header is treated as 1 (fail
	// toward "behave like the first attempt", never toward unconditional
	// refresh) — see the kernel-side parsing in Task 3.
	AttemptCount int

	// Headers are this specific attempt's outgoing request headers, mutable
	// via the returned UpstreamAttemptAction.
	Headers *Headers

	// Body is this specific attempt's outgoing request body — the ORIGINAL
	// client-sent bytes, replayed fresh by Envoy on every attempt (verified:
	// Envoy does not carry forward a previous attempt's mutation). Nil
	// unless the backing cluster's ext_proc filter has RequestBodyMode:
	// BUFFERED enabled (only clusters backing a model-failover route need
	// this — see go-network-service-hardening.md on not widening buffering
	// unnecessarily). A header-only consumer (oauth2-generator) never reads
	// this field and is unaffected by its addition.
	Body *Body
}
```

In `action.go`, extend the existing struct:

```go
// UpstreamAttemptHeaderModifications sets the given headers on this specific
// upstream attempt. An empty/nil HeadersToSet is a valid, common no-op (e.g.
// AttemptCount == 1, nothing to refresh yet, or a fail-open path after an
// error).
type UpstreamAttemptHeaderModifications struct {
	HeadersToSet map[string]string

	// Body replaces this attempt's outgoing request body when non-nil. Only
	// meaningful when UpstreamAttemptContext.Body was non-nil (the kernel
	// buffers the body for this attempt) — setting it otherwise is a no-op,
	// not an error. The kernel — not the caller — sets Content-Length to
	// match the replacement, matching setContentLengthHeader's existing
	// downstream-body-path convention; policies must never do this
	// themselves (a mismatched Content-Length makes Envoy reject the
	// mutation outright before the backend is ever dialed — verified in
	// the model-failover design spec's spike).
	Body []byte
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run 'TestUpstreamAttemptContext_BodyFieldExists|TestUpstreamAttemptHeaderModifications_BodyFieldExists' -v`
Expected: PASS.

- [ ] **Step 5: Run the full SDK test suite to confirm no regressions**

Run: `cd sdk/core && go build ./... && go test ./... 2>&1 | tail -40`
Expected: all PASS — in particular, any existing `oauth2-generator`-style test constructing `UpstreamAttemptHeaderModifications{HeadersToSet: ...}` without `Body` must still compile and pass unchanged (Go zero-value for a new field is `nil`, so this is guaranteed, but confirm).

- [ ] **Step 6: Commit**

```bash
cd sdk/core
git add policy/v1alpha2/context.go policy/v1alpha2/action.go policy/v1alpha2/upstream_attempt_test.go
git commit -m "feat(sdk): add Body support to the upstream-attempt phase for model-failover"
```

---

## Phase 2: Kernel — body-buffering ext_proc phase

### Task 3: Add a per-cluster body capability flag and `processRequestBody` to `UpstreamExternalProcessorServer`

**Files:**
- Modify: `gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go`
- Test: `gateway-runtime/policy-engine/internal/kernel/upstream_extproc_test.go`

**Interfaces:**
- Consumes: `policy.UpstreamAttemptContext`/`Body`/`UpstreamAttemptHeaderModifications` (Task 2); the existing `extractRouteKeyFromAttributes` (`extproc.go:522`, shared, unchanged); the existing `setContentLengthHeader` helper (`translator.go:1810`, currently used only by the downstream body path — Step 3 below calls it from here too, its first upstream-side caller).
- Produces: `Process` now handles `ProcessingRequest_RequestBody` in addition to `ProcessingRequest_RequestHeaders`, on the same stream, carrying `attemptCount` parsed at the headers message forward to the body message (per-`Process()`-call local state — this mirrors exactly what the throwaway spike in the design spec proved necessary and sufficient; do not invent per-stream server-side session storage, a local variable in the existing `for` loop is enough since both messages arrive on the same gRPC stream for the same attempt).

- [ ] **Step 1: Write the failing test**

```go
// added to upstream_extproc_test.go — follow this file's existing convention
// for constructing a fake stream (check existing tests around
// processRequestHeaders for the mock ExternalProcessor_ProcessServer shape
// already used here; reuse it rather than inventing a new one).
func TestProcess_RequestBodyAppliesBodyMutationWithContentLength(t *testing.T) {
	k := newTestKernelWithPolicyChain(t, "test-route", &fakeBodyMutatingPolicy{
		wantBody: []byte(`{"model":"gpt-4o-mini","marker":"attempt-2"}`),
	}) // fakeBodyMutatingPolicy implements policy.UpstreamAttemptPolicy and
	   // always returns UpstreamAttemptHeaderModifications{Body: wantBody}
	   // regardless of attempt count — add this small fake type in this
	   // test file, mirroring any existing fake *AttemptPolicy already used
	   // for the headers-only tests in this file.
	s := NewUpstreamExternalProcessorServer(k)

	stream := newFakeProcessServer(t, []*extprocv3.ProcessingRequest{
		requestHeadersMsg(t, "test-route", map[string]string{"x-envoy-attempt-count": "2"}),
		requestBodyMsg(t, []byte(`{"model":"gpt-4o","original":true}`)),
	}) // requestHeadersMsg/requestBodyMsg are small test-file helpers you add,
	   // constructing *extprocv3.ProcessingRequest values of each oneof case —
	   // model these on how upstream_extproc_test.go already builds a
	   // RequestHeaders ProcessingRequest for the existing header-only tests.

	if err := s.Process(stream); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	bodyResp := stream.sentResponses[1].GetRequestBody()
	if bodyResp == nil {
		t.Fatalf("expected a RequestBody response for the second message, got %#v", stream.sentResponses[1])
	}
	mutation := bodyResp.GetResponse().GetBodyMutation().GetBody()
	if string(mutation) != `{"model":"gpt-4o-mini","marker":"attempt-2"}` {
		t.Errorf("expected mutated body, got %q", mutation)
	}
	cl := headerValue(bodyResp.GetResponse().GetHeaderMutation(), "content-length") // small helper: find a header by key in a HeaderMutation.SetHeaders
	if cl != strconv.Itoa(len(mutation)) {
		t.Errorf("expected content-length %d, got %q", len(mutation), cl)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway-runtime/policy-engine && GOWORK=off go test ./internal/kernel/... -run TestProcess_RequestBodyAppliesBodyMutationWithContentLength -v`
Expected: FAIL — either a compile error (helpers not yet added) or the response has no body mutation because `Process` doesn't dispatch `RequestBody` messages yet.

- [ ] **Step 3: Implement.** Replace the existing `Process` method and `processRequestHeaders` in `upstream_extproc.go`:

```go
// Process implements extprocv3.ExternalProcessorServer. Two message types are
// possible now: RequestHeaders (every attempt) and RequestBody (only for
// clusters whose ext_proc filter has RequestBodyMode: BUFFERED — see Task 6;
// a headers-only cluster, e.g. plain retry-refresh, never sends this second
// message at all). attemptCount is parsed once from the headers message and
// reused for the body message on the same stream — both arrive for the same
// individual dial attempt.
func (s *UpstreamExternalProcessorServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	var routeKey string
	attemptCount := 1

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		var resp *extprocv3.ProcessingResponse
		switch v := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			routeKey = extractRouteKeyFromAttributes(req)
			resp, attemptCount, err = s.processRequestHeaders(ctx, req, routeKey)
			if err != nil {
				slog.ErrorContext(ctx, "upstream ext_proc: failed to process request headers, failing open", "error", err)
				resp = emptyContinueRequestHeadersResponse()
			}
		case *extprocv3.ProcessingRequest_RequestBody:
			resp, err = s.processRequestBody(ctx, v.RequestBody, routeKey, attemptCount)
			if err != nil {
				slog.ErrorContext(ctx, "upstream ext_proc: failed to process request body, failing open", "error", err)
				resp = emptyContinueRequestBodyResponse()
			}
		default:
			resp = emptyContinueRequestHeadersResponse()
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Internal, "upstream ext_proc: failed to send response: %v", err)
		}
	}
}

// emptyContinueRequestBodyResponse is the fail-open / no-op response for the
// body phase: no body mutation, the original buffered bytes proceed as-is.
func emptyContinueRequestBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
		},
	}
}

// processRequestHeaders is unchanged in behavior from before this task, with
// two differences: routeKey is now passed in (Process resolves it once and
// reuses it for the body message too, rather than each phase re-deriving it),
// and it now returns the parsed attemptCount so Process can carry it forward.
func (s *UpstreamExternalProcessorServer) processRequestHeaders(ctx context.Context, req *extprocv3.ProcessingRequest, routeKey string) (*extprocv3.ProcessingResponse, int, error) {
	chain := s.kernel.GetPolicyChain(routeKey)
	if chain == nil {
		return emptyContinueRequestHeadersResponse(), 1, nil
	}

	headers := req.GetRequestHeaders()
	attemptCount := 1
	headersMap := make(map[string][]string)
	if headers.GetHeaders() != nil {
		for _, h := range headers.GetHeaders().GetHeaders() {
			key := h.Key
			value := string(h.RawValue)
			headersMap[key] = append(headersMap[key], value)
			if key == "x-envoy-attempt-count" {
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					attemptCount = n
				}
			}
		}
	}

	actx := &policy.UpstreamAttemptContext{
		AttemptCount: attemptCount,
		Headers:      policy.NewHeaders(headersMap),
	}

	headersToSet := make(map[string]string)
	for _, p := range chain.Policies {
		attemptPolicy, ok := p.(policy.UpstreamAttemptPolicy)
		if !ok {
			continue
		}
		action := attemptPolicy.OnUpstreamAttemptRequestHeaders(ctx, actx)
		mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
		if !ok {
			continue
		}
		for k, v := range mods.HeadersToSet {
			headersToSet[k] = v
		}
	}

	if len(headersToSet) == 0 {
		return emptyContinueRequestHeadersResponse(), attemptCount, nil
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: buildHeaderValueOptions(headersToSet),
				},
			},
		},
	}, attemptCount, nil
}

// processRequestBody dispatches the SAME OnUpstreamAttemptRequestHeaders hook
// again (not a new interface method — see the SDK design note in Task 2),
// this time with Body populated, and applies any returned Body mutation with
// a matching Content-Length. Policies that never set actx.Body-derived logic
// (headers-only consumers like oauth2-generator) simply won't have this
// method invoked at all for their clusters, since their clusters never
// enable RequestBodyMode — this handler only ever runs for clusters that do.
func (s *UpstreamExternalProcessorServer) processRequestBody(ctx context.Context, body *extprocv3.HttpBody, routeKey string, attemptCount int) (*extprocv3.ProcessingResponse, error) {
	chain := s.kernel.GetPolicyChain(routeKey)
	if chain == nil {
		return emptyContinueRequestBodyResponse(), nil
	}

	actx := &policy.UpstreamAttemptContext{
		AttemptCount: attemptCount,
		Body:         &policy.Body{Content: body.GetBody(), Present: true, EndOfStream: true},
	}

	var mutatedBody []byte
	for _, p := range chain.Policies {
		attemptPolicy, ok := p.(policy.UpstreamAttemptPolicy)
		if !ok {
			continue
		}
		action := attemptPolicy.OnUpstreamAttemptRequestHeaders(ctx, actx)
		mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
		if !ok || mods.Body == nil {
			continue
		}
		mutatedBody = mods.Body // last write wins, matching HeadersToSet's own convention
	}

	if mutatedBody == nil {
		return emptyContinueRequestBodyResponse(), nil
	}

	headerMutation := buildHeaderValueOptions(map[string]string{})
	setContentLengthHeader(headerMutation, len(mutatedBody))

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation:   &extprocv3.BodyMutation{Mutation: &extprocv3.BodyMutation_Body{Body: mutatedBody}},
					HeaderMutation: headerMutation,
				},
			},
		},
	}, nil
}
```

Note: `buildHeaderValueOptions(map[string]string{})` returns `nil` per its existing `if len(headers) == 0 { return nil }` guard (`translator.go:1789`) — confirm `setContentLengthHeader` handles a `nil` mutation input by allocating (it already does, per its existing body: `if mutation.SetHeaders == nil { mutation.SetHeaders = make(...) }` — but that line dereferences `mutation` first, so if `buildHeaderValueOptions` returns a `nil` *pointer*, this panics). Fix by allocating the mutation directly here instead of going through `buildHeaderValueOptions` for the empty case:

```go
headerMutation := &extprocv3.HeaderMutation{}
setContentLengthHeader(headerMutation, len(mutatedBody))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-runtime/policy-engine && GOWORK=off go test ./internal/kernel/... -run TestProcess_RequestBodyAppliesBodyMutationWithContentLength -v`
Expected: PASS.

- [ ] **Step 5: Run the full kernel test suite to confirm no regressions**

Run: `cd gateway-runtime/policy-engine && GOWORK=off go build ./... && GOWORK=off go test ./internal/kernel/... -v 2>&1 | tail -100`
Expected: all PASS — in particular every existing headers-only `UpstreamAttemptPolicy` test (retry-refresh/oauth2-generator) must still pass unchanged, since `processRequestHeaders`'s behavior for a chain with no `Body`-setting policy is identical to before this task.

- [ ] **Step 6: Commit**

```bash
cd gateway-runtime/policy-engine
git add internal/kernel/upstream_extproc.go internal/kernel/upstream_extproc_test.go
git commit -m "feat(policy-engine): add request-body phase to the upstream ext_proc server for model-failover"
```

---

## Phase 3: Translator — aggregate cluster, retry_priority, and the body-enabled filter

### Task 4: Parse `model-failover` policy params and reject it alongside `resilience.retry`

**Files:**
- Modify: `gateway-controller/pkg/config/llm_validator.go`
- Test: `gateway-controller/pkg/config/llm_validator_test.go`

**Interfaces:**
- Consumes: `models.Policy{Name, Version, Params map[string]interface{}}` (`pkg/models/runtime_deploy_config.go:113-118`); `api.Resilience`/`api.Retry` (existing, `ResolveResilience` at `pkg/xds/translator.go:3517`).
- Produces: `type ModelFailoverParams struct { Models []ModelFailoverTarget; StatusCodes []int; RequestTimeout *time.Duration; SuspendDuration *time.Duration; CacheStrategy string }` and `type ModelFailoverTarget struct { Name string; UpstreamDefinition string }`, plus `func ParseModelFailoverParams(params map[string]interface{}) (*ModelFailoverParams, error)` and `func ValidateModelFailoverPolicy(mf *ModelFailoverParams, retry *api.Retry) error` — both exported from `pkg/config`, consumed by Task 5/6 in `pkg/xds`.

- [ ] **Step 1: Write the failing test**

```go
// added to llm_validator_test.go
func TestParseModelFailoverParams_ValidConfig(t *testing.T) {
	params := map[string]interface{}{
		"models": []interface{}{
			map[string]interface{}{"name": "gpt-4o", "upstreamDefinition": "primary"},
			map[string]interface{}{"name": "gpt-4o-mini", "upstreamDefinition": "fallback-1"},
		},
		"statusCodes":     []interface{}{429, 500, 502, 503, 504},
		"requestTimeout":  "10s",
		"suspendDuration": "30s",
	}

	mf, err := ParseModelFailoverParams(params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mf.Models) != 2 || mf.Models[0].Name != "gpt-4o" || mf.Models[0].UpstreamDefinition != "primary" {
		t.Errorf("unexpected Models: %#v", mf.Models)
	}
	if len(mf.StatusCodes) != 5 || mf.StatusCodes[0] != 429 {
		t.Errorf("unexpected StatusCodes: %v", mf.StatusCodes)
	}
	if mf.RequestTimeout == nil || *mf.RequestTimeout != 10*time.Second {
		t.Errorf("unexpected RequestTimeout: %v", mf.RequestTimeout)
	}
	if mf.SuspendDuration == nil || *mf.SuspendDuration != 30*time.Second {
		t.Errorf("unexpected SuspendDuration: %v", mf.SuspendDuration)
	}
}

func TestParseModelFailoverParams_RequiresAtLeastTwoModels(t *testing.T) {
	params := map[string]interface{}{
		"models": []interface{}{
			map[string]interface{}{"name": "gpt-4o", "upstreamDefinition": "primary"},
		},
		"statusCodes": []interface{}{500},
	}
	if _, err := ParseModelFailoverParams(params); err == nil {
		t.Error("expected an error for a single-target models list — nothing to fail over to")
	}
}

func TestParseModelFailoverParams_RequiresNonEmptyStatusCodes(t *testing.T) {
	params := map[string]interface{}{
		"models": []interface{}{
			map[string]interface{}{"name": "gpt-4o", "upstreamDefinition": "primary"},
			map[string]interface{}{"name": "gpt-4o-mini", "upstreamDefinition": "fallback-1"},
		},
		"statusCodes": []interface{}{},
	}
	if _, err := ParseModelFailoverParams(params); err == nil {
		t.Error("expected an error for empty statusCodes")
	}
}

func TestValidateModelFailoverPolicy_RejectsCoexistenceWithResilienceRetry(t *testing.T) {
	mf := &ModelFailoverParams{
		Models:      []ModelFailoverTarget{{Name: "a", UpstreamDefinition: "x"}, {Name: "b", UpstreamDefinition: "y"}},
		StatusCodes: []int{500},
	}
	retry := &api.Retry{StatusCodes: []int{401}}
	if err := ValidateModelFailoverPolicy(mf, retry); err == nil {
		t.Error("expected an error when both model-failover and resilience.retry are configured on the same route")
	}
	if err := ValidateModelFailoverPolicy(mf, nil); err != nil {
		t.Errorf("expected no error when resilience.retry is absent, got: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/config/... -run 'TestParseModelFailoverParams|TestValidateModelFailoverPolicy' -v`
Expected: FAIL with `undefined: ParseModelFailoverParams` (and friends).

- [ ] **Step 3: Implement.** New file `gateway-controller/pkg/config/model_failover_validator.go`:

```go
package config

import (
	"fmt"
	"time"

	api "github.com/wso2/api-platform/gateway-controller/pkg/api/management"
)

// ModelFailoverTarget is one entry in model-failover's ordered fallback chain.
// Models[0] is the primary; index i corresponds to Envoy attempt i+1 — see
// the "AttemptCount alone is sufficient" note in the design spec.
type ModelFailoverTarget struct {
	Name               string // injected into the request body's "model" field for this target
	UpstreamDefinition string // must match an api.UpstreamDefinition.Name declared on the same API
}

// ModelFailoverParams is the parsed, validated shape of the model-failover
// policy's policyParams. RequestTimeout/SuspendDuration are nil when unset
// (SuspendDuration nil means "no suspend tracking at all" — see the design
// spec's cache.strategy note).
type ModelFailoverParams struct {
	Models          []ModelFailoverTarget
	StatusCodes     []int
	RequestTimeout  *time.Duration
	SuspendDuration *time.Duration
	CacheStrategy   string // "memory" (default) or "redis"; only consulted when SuspendDuration != nil
}

// ParseModelFailoverParams parses and validates model-failover's policyParams
// map (as stored in models.Policy.Params). numRetries is deliberately not a
// field here — the translator derives it as len(Models)-1 (Task 6), so it
// can never drift out of sync with the fallback list.
func ParseModelFailoverParams(params map[string]interface{}) (*ModelFailoverParams, error) {
	rawModels, ok := params["models"].([]interface{})
	if !ok || len(rawModels) < 2 {
		return nil, fmt.Errorf("model-failover requires at least 2 entries in 'models' (a primary plus at least one fallback), got %d", len(rawModels))
	}

	models := make([]ModelFailoverTarget, 0, len(rawModels))
	for i, raw := range rawModels {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("model-failover: models[%d] is not an object", i)
		}
		name, _ := m["name"].(string)
		def, _ := m["upstreamDefinition"].(string)
		if name == "" {
			return nil, fmt.Errorf("model-failover: models[%d].name is required", i)
		}
		if def == "" {
			return nil, fmt.Errorf("model-failover: models[%d].upstreamDefinition is required", i)
		}
		models = append(models, ModelFailoverTarget{Name: name, UpstreamDefinition: def})
	}

	rawStatusCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawStatusCodes) == 0 {
		return nil, fmt.Errorf("model-failover requires a non-empty 'statusCodes' list")
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for _, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok { // YAML/JSON numeric decode may hand back float64
				code = int(f)
			} else {
				return nil, fmt.Errorf("model-failover: statusCodes entries must be integers")
			}
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("model-failover: statusCodes entry %d is not a valid HTTP status code", code)
		}
		statusCodes = append(statusCodes, code)
	}

	mf := &ModelFailoverParams{Models: models, StatusCodes: statusCodes, CacheStrategy: "memory"}

	if raw, ok := params["requestTimeout"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid requestTimeout %q: %w", raw, err)
		}
		mf.RequestTimeout = &d
	}
	if raw, ok := params["suspendDuration"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid suspendDuration %q: %w", raw, err)
		}
		mf.SuspendDuration = &d
	}
	if cache, ok := params["cache"].(map[string]interface{}); ok {
		if strategy, ok := cache["strategy"].(string); ok && strategy != "" {
			if strategy != "memory" && strategy != "redis" {
				return nil, fmt.Errorf("model-failover: cache.strategy must be 'memory' or 'redis', got %q", strategy)
			}
			mf.CacheStrategy = strategy
		}
	}

	return mf, nil
}

// ValidateModelFailoverPolicy rejects a route that configures both
// model-failover and resilience.retry — see Global Constraints in this
// feature's implementation plan for why they're mutually exclusive in v1.
func ValidateModelFailoverPolicy(mf *ModelFailoverParams, retry *api.Retry) error {
	if mf == nil {
		return nil
	}
	if retry != nil {
		return fmt.Errorf("model-failover cannot be combined with resilience.retry on the same route/operation — they both drive RouteAction.RetryPolicy")
	}
	return nil
}
```

Check the actual import path for `api` in `llm_validator.go` (the alias/package this repo already uses for `pkg/api/management`) and match it exactly — do not introduce a second alias for the same package.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/config/... -run 'TestParseModelFailoverParams|TestValidateModelFailoverPolicy' -v`
Expected: PASS.

- [ ] **Step 5: Run the full config package test suite**

Run: `cd gateway-controller && go build ./... && go test ./pkg/config/... -v 2>&1 | tail -60`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd gateway-controller
git add pkg/config/model_failover_validator.go pkg/config/llm_validator_test.go
git commit -m "feat(gateway-controller): parse and validate model-failover policy params"
```

### Task 5: Resolve `upstreamDefinition` references to cluster names and build the aggregate cluster

**Files:**
- Modify: `gateway-controller/pkg/xds/translator.go`
- Test: `gateway-controller/pkg/xds/translator_test.go`

**Interfaces:**
- Consumes: `config.ModelFailoverParams`/`ModelFailoverTarget` (Task 4); the existing per-`upstreamDefinition` cluster naming convention confirmed in Task 1 (`constants.UpstreamDefinitionClusterPrefix + kind + "_" + apiId + "_" + sanitizeUpstreamDefinitionName(name)`, `pkg/transform/restapi.go:245`); `aggregatev3.ClusterConfig` from `github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3`.
- Produces: `func (t *Translator) createAggregateCluster(name string, memberClusterNames []string) (*cluster.Cluster, error)` and `func modelFailoverMemberClusterNames(mf *config.ModelFailoverParams, apiKind, apiID string) []string` — both consumed by Task 6.

- [ ] **Step 1: Write the failing test**

```go
// added to translator_test.go
func TestModelFailoverMemberClusterNames_ResolvesInOrder(t *testing.T) {
	mf := &config.ModelFailoverParams{
		Models: []config.ModelFailoverTarget{
			{Name: "gpt-4o", UpstreamDefinition: "primary"},
			{Name: "gpt-4o-mini", UpstreamDefinition: "fallback-1"},
		},
	}
	names := modelFailoverMemberClusterNames(mf, "LlmProvider", "abc-123")
	want := []string{"upstream_LlmProvider_abc-123_primary", "upstream_LlmProvider_abc-123_fallback-1"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("got %v, want %v", names, want)
	}
}

func TestCreateAggregateCluster_ListsMembersInOrder(t *testing.T) {
	tr := &Translator{logger: slog.Default()}
	c, err := tr.createAggregateCluster("agg_test", []string{"upstream_a", "upstream_b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Name != "agg_test" {
		t.Errorf("unexpected cluster name: %q", c.Name)
	}
	if c.LbPolicy != cluster.Cluster_CLUSTER_PROVIDED {
		t.Errorf("expected CLUSTER_PROVIDED lb_policy, got %v", c.LbPolicy)
	}
	ct, ok := c.ClusterDiscoveryType.(*cluster.Cluster_ClusterType)
	if !ok {
		t.Fatalf("expected Cluster_ClusterType, got %T", c.ClusterDiscoveryType)
	}
	if ct.ClusterType.Name != "envoy.clusters.aggregate" {
		t.Errorf("unexpected cluster_type name: %q", ct.ClusterType.Name)
	}
	var cfg aggregatev3.ClusterConfig
	if err := ct.ClusterType.TypedConfig.UnmarshalTo(&cfg); err != nil {
		t.Fatalf("failed to unmarshal ClusterConfig: %v", err)
	}
	if len(cfg.Clusters) != 2 || cfg.Clusters[0] != "upstream_a" || cfg.Clusters[1] != "upstream_b" {
		t.Errorf("unexpected member order: %v", cfg.Clusters)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/xds/... -run 'TestModelFailoverMemberClusterNames|TestCreateAggregateCluster' -v`
Expected: FAIL with `undefined: modelFailoverMemberClusterNames` / `undefined: (*Translator).createAggregateCluster`.

- [ ] **Step 3: Implement.** Add to `translator.go`, near `createWeightedCluster` (so the three cluster-construction functions stay together):

Add the import (alongside this file's existing `cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"` etc.):

```go
aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
```

```go
// modelFailoverMemberClusterNames resolves each target's upstreamDefinition
// reference to the exact cluster name RestAPITransformer already creates for
// it (constants.UpstreamDefinitionClusterPrefix + kind + "_" + apiID + "_" +
// sanitizeUpstreamDefinitionName(name)) — the SAME clusters upstreamDefinitions
// always produces, never a new/parallel set. Order is preserved: index i
// here corresponds to Envoy attempt i+1 (see the design spec's AttemptCount
// note) — this function must never reorder or dedupe.
func modelFailoverMemberClusterNames(mf *config.ModelFailoverParams, apiKind, apiID string) []string {
	names := make([]string, len(mf.Models))
	for i, m := range mf.Models {
		names[i] = constants.UpstreamDefinitionClusterPrefix + apiKind + "_" + apiID + "_" + sanitizeUpstreamDefinitionName(m.UpstreamDefinition)
	}
	return names
}

// createAggregateCluster builds an envoy.clusters.aggregate cluster composing
// memberClusterNames in priority order (index 0 = highest priority — Envoy's
// aggregate cluster assigns priorities by list position). lb_policy MUST be
// CLUSTER_PROVIDED: the aggregate cluster type supplies its own load
// balancing and rejects any other value. This cluster carries no endpoints
// of its own — connections are always established through whichever member
// cluster the aggregate's priority/retry logic selects.
func (t *Translator) createAggregateCluster(name string, memberClusterNames []string) (*cluster.Cluster, error) {
	cfg := &aggregatev3.ClusterConfig{Clusters: memberClusterNames}
	cfgAny, err := anypb.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal aggregate cluster config for %q: %w", name, err)
	}
	return &cluster.Cluster{
		Name:     name,
		LbPolicy: cluster.Cluster_CLUSTER_PROVIDED,
		ClusterDiscoveryType: &cluster.Cluster_ClusterType{
			ClusterType: &cluster.CustomClusterType{
				Name:        "envoy.clusters.aggregate",
				TypedConfig: cfgAny,
			},
		},
	}, nil
}
```

Confirm the exact import alias this file already uses for `pkg/config` (for `config.ModelFailoverParams`) — match it, don't introduce a second one.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/xds/... -run 'TestModelFailoverMemberClusterNames|TestCreateAggregateCluster' -v`
Expected: PASS.

- [ ] **Step 5: Run the full xds package test suite**

Run: `cd gateway-controller && go build ./... && go test ./pkg/xds/... -v 2>&1 | tail -80`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd gateway-controller
git add pkg/xds/translator.go pkg/xds/translator_test.go
git commit -m "feat(gateway-controller): build an aggregate cluster from model-failover's upstreamDefinition targets"
```

### Task 6: Wire the aggregate cluster into route resolution, `RetryPolicy` with `retry_priority`, and the body-enabled ext_proc filter

**Files:**
- Modify: `gateway-controller/pkg/xds/translator.go`
- Test: `gateway-controller/pkg/xds/translator_test.go`

**Interfaces:**
- Consumes: `createAggregateCluster`/`modelFailoverMemberClusterNames` (Task 5); the existing `buildRetryPolicy` (`translator.go:3497`, extended, not replaced); the existing `collectClustersNeedingUpstreamFilter`/`attachUpstreamRefreshFilter` pattern (`translator.go:1037`/`1056`, mirrored with a second, distinct set for body-needing clusters — never merged into the same set, per Global Constraints); `previous_prioritiesv3.PreviousPrioritiesConfig` from `github.com/envoyproxy/go-control-plane/envoy/extensions/retry/priority/previous_priorities/v3`.
- Produces: for a route whose `PolicyChain` includes a `model-failover` policy, the route's `RouteUpstream.ClusterKey`/`DefaultCluster` (`pkg/models/runtime_deploy_config.go:96-105`) is set to the aggregate cluster's name instead of the route's normal upstream, `RouteAction.RetryPolicy` gets `RetryPriority` + `PerTryTimeout`, and `VirtualHost.IncludeRequestAttemptCount` is set true for that vhost (reusing the existing per-vhost scoping already fixed for retry-refresh — do not reintroduce a global/unscoped version of this).

- [ ] **Step 1: Write the failing test**

```go
// added to translator_test.go
func TestBuildRetryPolicyWithPriority_SetsRetryPriorityAndPerTryTimeout(t *testing.T) {
	timeout := 10 * time.Second
	mf := &config.ModelFailoverParams{
		Models:         []config.ModelFailoverTarget{{Name: "a", UpstreamDefinition: "x"}, {Name: "b", UpstreamDefinition: "y"}, {Name: "c", UpstreamDefinition: "z"}},
		StatusCodes:    []int{500, 502},
		RequestTimeout: &timeout,
	}

	rp := buildModelFailoverRetryPolicy(mf)

	if rp.GetNumRetries().GetValue() != 2 { // len(Models) - 1
		t.Errorf("expected NumRetries 2, got %v", rp.GetNumRetries())
	}
	if len(rp.RetriableStatusCodes) != 2 || rp.RetriableStatusCodes[0] != 500 {
		t.Errorf("unexpected RetriableStatusCodes: %v", rp.RetriableStatusCodes)
	}
	if rp.GetRetryPriority().GetName() != "envoy.retry_priorities.previous_priorities" {
		t.Errorf("expected previous_priorities retry_priority, got %v", rp.GetRetryPriority())
	}
	if rp.GetPerTryTimeout().AsDuration() != 10*time.Second {
		t.Errorf("expected per_try_timeout 10s, got %v", rp.GetPerTryTimeout())
	}
}

func TestCollectClustersNeedingUpstreamBodyFilter_OnlyMarksAggregateClusters(t *testing.T) {
	routes := []*route.Route{
		{Action: &route.Route_Route{Route: &route.RouteAction{
			ClusterSpecifier: &route.RouteAction_Cluster{Cluster: "agg_modelfailover_1"},
			RetryPolicy:      &route.RetryPolicy{RetryPriority: &route.RetryPolicy_RetryPriority{Name: "envoy.retry_priorities.previous_priorities"}},
		}}},
		{Action: &route.Route_Route{Route: &route.RouteAction{ // plain resilience.retry route — must NOT be marked
			ClusterSpecifier: &route.RouteAction_Cluster{Cluster: "plain_retry_cluster"},
			RetryPolicy:      &route.RetryPolicy{},
		}}},
	}
	dest := make(map[string]bool)
	collectClustersNeedingUpstreamBodyFilter(routes, dest)
	if !dest["agg_modelfailover_1"] {
		t.Error("expected the aggregate/retry_priority route's cluster to be marked")
	}
	if dest["plain_retry_cluster"] {
		t.Error("a plain resilience.retry route (no retry_priority) must not be marked for body buffering")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/xds/... -run 'TestBuildRetryPolicyWithPriority|TestCollectClustersNeedingUpstreamBodyFilter' -v`
Expected: FAIL with `undefined: buildModelFailoverRetryPolicy` / `undefined: collectClustersNeedingUpstreamBodyFilter`.

- [ ] **Step 3a: Implement `buildModelFailoverRetryPolicy` and `collectClustersNeedingUpstreamBodyFilter`.** Add near `buildRetryPolicy`/`collectClustersNeedingUpstreamFilter`:

```go
previous_prioritiesv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/priority/previous_priorities/v3"
```

```go
// buildModelFailoverRetryPolicy mirrors buildRetryPolicy but derives
// NumRetries from len(Models)-1 (never a separately configured knob — see
// the design spec) and adds RetryPriority so Envoy prefers a
// not-yet-attempted priority (i.e. the next fallback in the aggregate
// cluster) on each retry, plus PerTryTimeout from RequestTimeout when set.
func buildModelFailoverRetryPolicy(mf *config.ModelFailoverParams) *route.RetryPolicy {
	statusCodes := make([]uint32, len(mf.StatusCodes))
	for i, code := range mf.StatusCodes {
		statusCodes[i] = uint32(code)
	}

	priorityCfgAny, err := anypb.New(&previous_prioritiesv3.PreviousPrioritiesConfig{UpdateFrequency: 1})
	if err != nil {
		// anypb.New only fails on a marshal error for a well-formed proto message,
		// which PreviousPrioritiesConfig{UpdateFrequency: 1} can never produce —
		// treated as unreachable, matching this file's existing convention of not
		// threading an error return through every proto-marshal call site (see
		// createUpstreamRefreshExtProcFilter for the same pattern).
		priorityCfgAny = nil
	}

	rp := &route.RetryPolicy{
		RetryOn:              "retriable-status-codes",
		RetriableStatusCodes: statusCodes,
		NumRetries:           wrapperspb.UInt32(uint32(len(mf.Models) - 1)),
		RetryPriority: &route.RetryPolicy_RetryPriority{
			Name:                "envoy.retry_priorities.previous_priorities",
			ConfigType:          &route.RetryPolicy_RetryPriority_TypedConfig{TypedConfig: priorityCfgAny},
		},
	}
	if mf.RequestTimeout != nil {
		rp.PerTryTimeout = durationpb.New(*mf.RequestTimeout)
	}
	return rp
}

// collectClustersNeedingUpstreamBodyFilter is the model-failover-specific
// counterpart to collectClustersNeedingUpstreamFilter — deliberately a
// SEPARATE set, never merged into it. A plain resilience.retry route's
// cluster must never get RequestBodyMode: BUFFERED (unnecessary buffering
// cost); only a route whose RetryPolicy carries RetryPriority (which only
// model-failover routes ever set — see Task 6 Step 3b) is marked here.
func collectClustersNeedingUpstreamBodyFilter(routes []*route.Route, dest map[string]bool) {
	for _, r := range routes {
		ra := r.GetRoute()
		if ra == nil || ra.GetRetryPolicy() == nil || ra.GetRetryPolicy().GetRetryPriority() == nil {
			continue
		}
		if clusterName := ra.GetCluster(); clusterName != "" {
			dest[clusterName] = true
		}
	}
}
```

- [ ] **Step 3b: Wire aggregate-cluster resolution into route/cluster building.** In `translateRuntimeConfig` (`translator.go:253`), after the existing `UpstreamClusters` loop and before route building, add:

```go
// model-failover: for any route whose PolicyChain includes it, build an
// aggregate cluster from its resolved upstreamDefinition targets and point
// the route at it instead of its normal upstream. Mutual exclusivity with
// resilience.retry is already enforced at validation time (Task 4), so a
// route reaching here with model-failover configured never also has
// rdc.Routes[key].Timeout.Retry set.
modelFailoverClusterByRouteKey := make(map[string]string)
for routeKey, chain := range rdc.PolicyChains {
	for _, p := range chain.Policies {
		if p.Name != "model-failover" {
			continue
		}
		mf, err := config.ParseModelFailoverParams(p.Params)
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: %w", routeKey, err)
		}
		memberNames := modelFailoverMemberClusterNames(mf, rdc.Metadata.Kind, rdc.Metadata.UUID)
		aggName := "agg_modelfailover_" + rdc.Metadata.Kind + "_" + rdc.Metadata.UUID + "_" + sanitizeUpstreamDefinitionName(routeKey)
		aggCluster, err := t.createAggregateCluster(aggName, memberNames)
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: %w", routeKey, err)
		}
		clusters = append(clusters, aggCluster)
		modelFailoverClusterByRouteKey[routeKey] = aggName
		break // a route has at most one model-failover policy — validated elsewhere, not re-checked here
	}
}
```

Then, at the point where each route's `RouteAction` is built from `rdcRoute.Upstream` (the same function containing the `routeResilienceRetry` block at line 353), branch on `modelFailoverClusterByRouteKey`:

```go
if aggName, ok := modelFailoverClusterByRouteKey[routeKey]; ok {
	routeAction.Route.ClusterSpecifier = &route.RouteAction_Cluster{Cluster: aggName}
	for _, p := range rdc.PolicyChains[routeKey].Policies {
		if p.Name == "model-failover" {
			if mf, err := config.ParseModelFailoverParams(p.Params); err == nil {
				routeAction.Route.RetryPolicy = buildModelFailoverRetryPolicy(mf)
			}
			break
		}
	}
} else if rdcRoute.Upstream.UseClusterHeader {
	routeAction.Route.ClusterSpecifier = &route.RouteAction_ClusterHeader{
		ClusterHeader: constants.TargetUpstreamHeader,
	}
	// ... existing UseClusterHeader handling continues unchanged below this point —
	// do not duplicate it, just don't let the model-failover branch fall through to it.
} else if routeResilienceRetry != nil {
	// ... existing non-model-failover cluster-specifier / RetryPolicy handling, unchanged
}
```

Adjust this to fit the exact existing `if`/`else if` chain at that call site precisely — read the surrounding ~40 lines before editing so the model-failover branch is inserted as a new leading case without altering the existing branches' behavior for non-model-failover routes.

- [ ] **Step 3c: Attach the body-enabled filter and set `IncludeRequestAttemptCount`.** Near the existing `clustersNeedingUpstreamFilter`/`attachUpstreamRefreshFilter` call site (`translator.go:724`, `789`, `792`):

```go
clustersNeedingUpstreamBodyFilter := make(map[string]bool)
// ... inside the same per-vhost loop that already calls
// collectClustersNeedingUpstreamFilter(routesList, clustersNeedingUpstreamFilter):
collectClustersNeedingUpstreamBodyFilter(routesList, clustersNeedingUpstreamBodyFilter)

// ... after the existing attachUpstreamRefreshFilter call:
if err := t.attachUpstreamBodyFilter(clusterMap, clustersNeedingUpstreamBodyFilter); err != nil {
	return nil, err
}
```

Add `attachUpstreamBodyFilter` and its filter constructor near `attachUpstreamRefreshFilter`/`createUpstreamRefreshExtProcFilter` — same shape, `RequestBodyMode: BUFFERED` added, same internal ext_proc target cluster (no new internal cluster needed — Task 3's kernel change lives in the same server):

```go
func (t *Translator) createUpstreamBodyExtProcFilter() (*hcm.HttpFilter, error) {
	policyEngine := t.routerConfig.PolicyEngine
	extProcConfig := &extproc.ExternalProcessor{
		GrpcService: &core.GrpcService{
			TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: constants.UpstreamRefreshPolicyEngineClusterName},
			},
			Timeout: durationpb.New(time.Duration(policyEngine.TimeoutMs) * time.Millisecond),
		},
		FailureModeAllow: true,
		ProcessingMode: &extproc.ProcessingMode{
			RequestHeaderMode: extproc.ProcessingMode_SEND,
			RequestBodyMode:   extproc.ProcessingMode_BUFFERED,
		},
		MessageTimeout:    durationpb.New(time.Duration(policyEngine.MessageTimeoutMs) * time.Millisecond),
		RequestAttributes: []string{constants.ExtProcRequestAttributeRouteName},
	}
	extProcAny, err := anypb.New(extProcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal upstream body ext_proc config: %w", err)
	}
	return &hcm.HttpFilter{
		Name:       constants.ExtProcFilterName + "_upstream_body",
		ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: extProcAny},
	}, nil
}

// attachUpstreamBodyFilter mirrors attachUpstreamRefreshFilter exactly (same
// upstream_codec-terminated filter chain requirement, same
// ExplicitHttpConfig/HTTP1ProtocolOptions choice) but with RequestBodyMode
// enabled and scoped to clustersNeedingUpstreamBodyFilter — a route's
// cluster is never in both this set and clustersNeedingUpstreamFilter (see
// Global Constraints), so no cluster ever receives two upstream ext_proc
// filters.
func (t *Translator) attachUpstreamBodyFilter(clusterMap map[string]*cluster.Cluster, clustersNeedingUpstreamBodyFilter map[string]bool) error {
	if len(clustersNeedingUpstreamBodyFilter) == 0 {
		return nil
	}

	upstreamFilter, err := t.createUpstreamBodyExtProcFilter()
	if err != nil {
		return fmt.Errorf("failed to create upstream body ext_proc filter: %w", err)
	}

	codecAny, err := anypb.New(&upstreamcodecv3.UpstreamCodec{})
	if err != nil {
		return fmt.Errorf("failed to marshal upstream codec filter: %w", err)
	}
	codecFilter := &hcm.HttpFilter{
		Name:       constants.UpstreamCodecFilterName,
		ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: codecAny},
	}

	protocolOptions := &httpv3.HttpProtocolOptions{
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{
					HttpProtocolOptions: &core.Http1ProtocolOptions{},
				},
			},
		},
		HttpFilters: []*hcm.HttpFilter{upstreamFilter, codecFilter},
	}
	protocolOptionsAny, err := anypb.New(protocolOptions)
	if err != nil {
		return fmt.Errorf("failed to marshal upstream HttpProtocolOptions for body filter: %w", err)
	}

	for clusterName := range clustersNeedingUpstreamBodyFilter {
		c, ok := clusterMap[clusterName]
		if !ok {
			continue
		}
		if c.TypedExtensionProtocolOptions == nil {
			c.TypedExtensionProtocolOptions = make(map[string]*anypb.Any)
		}
		c.TypedExtensionProtocolOptions[upstreamHTTPProtocolOptionsKey] = protocolOptionsAny
	}

	if _, ok := clusterMap[constants.UpstreamRefreshPolicyEngineClusterName]; !ok {
		clusterMap[constants.UpstreamRefreshPolicyEngineClusterName] = t.createUpstreamRefreshExtProcCluster()
	}

	return nil
}
```

Set `IncludeRequestAttemptCount` on the vhost for any model-failover route exactly where the existing per-vhost scoping fix for retry-refresh already lives (per `340387e4d`/`ed36f7ef0` — find that exact code site by searching for `IncludeRequestAttemptCount` in this file) and extend its condition to also cover `modelFailoverClusterByRouteKey`, not just `routeResilienceRetry != nil`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/xds/... -run 'TestBuildRetryPolicyWithPriority|TestCollectClustersNeedingUpstreamBodyFilter' -v`
Expected: PASS.

- [ ] **Step 5: Run the full xds package test suite and full build**

Run: `cd gateway-controller && go build ./... && go test ./pkg/xds/... -v 2>&1 | tail -150`
Expected: all PASS — pay particular attention to any existing test asserting a fixed total cluster/filter count for a deployment, since this task adds new clusters/filters conditionally; update any such count-based assertion to account for the model-failover case if one exists and breaks.

- [ ] **Step 6: Commit**

```bash
cd gateway-controller
git add pkg/xds/translator.go pkg/xds/translator_test.go
git commit -m "feat(gateway-controller): route model-failover operations through an aggregate cluster with retry_priority and body-mutation ext_proc"
```

---

## Phase 4: The `model-failover` policy itself — `gateway-controllers` first, then `dev-policies`

### Task 7: Scaffold `model-failover` in `gateway-controllers/policies/model-failover`

**Files:**
- Create (in the separate `gateway-controllers` checkout): `policies/model-failover/go.mod`, `policies/model-failover/policy-definition.yaml`, `policies/model-failover/model_failover.go`, `policies/model-failover/model_failover_test.go`

**Interfaces:**
- Consumes: `policy.PolicyMetadata`/`policy.GetPolicy` signature (copy the exact shape from `gateway-controllers/policies/oauth2-generator/oauth2_generator.go`'s own `GetPolicy` — same repo, same conventions, same `go.mod` `replace`/version pins for `sdk/core`).
- Produces: `func GetPolicy(metadata policy.PolicyMetadata, params map[string]interface{}) (policy.Policy, error)` returning a `*Policy` with parsed, validated fields — `models []modelTarget`, `statusCodes map[int]struct{}`, `requestTimeout time.Duration`, `suspendDuration time.Duration` (zero = disabled), `cacheStrategy string` — consumed by Tasks 8-10.

- [ ] **Step 1: Write the failing test**

```go
// model_failover_test.go
package modelfailover

import "testing"

func TestGetPolicy_ValidConfig(t *testing.T) {
	params := map[string]interface{}{
		"models": []interface{}{
			map[string]interface{}{"name": "gpt-4o", "upstreamDefinition": "primary"},
			map[string]interface{}{"name": "gpt-4o-mini", "upstreamDefinition": "fallback-1"},
		},
		"statusCodes": []interface{}{500, 502, 503},
	}
	p, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mfp, ok := p.(*Policy)
	if !ok {
		t.Fatalf("expected *Policy, got %T", p)
	}
	if len(mfp.models) != 2 || mfp.models[0].name != "gpt-4o" {
		t.Errorf("unexpected models: %#v", mfp.models)
	}
	if _, ok := mfp.statusCodes[500]; !ok {
		t.Error("expected 500 in statusCodes")
	}
}

func TestGetPolicy_RejectsSingleModel(t *testing.T) {
	params := map[string]interface{}{
		"models":      []interface{}{map[string]interface{}{"name": "gpt-4o", "upstreamDefinition": "primary"}},
		"statusCodes": []interface{}{500},
	}
	if _, err := GetPolicy(policy.PolicyMetadata{}, params); err == nil {
		t.Error("expected an error for a single-target models list")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd policies/model-failover && GOWORK=off go test ./... -v`
Expected: FAIL — package doesn't exist yet.

- [ ] **Step 3: Scaffold.** `go.mod` (copy the exact `go`/`replace`/`require` lines from `oauth2-generator`'s `go.mod` in this same repo, changing only the module path):

```go
module github.com/wso2/gateway-controllers/policies/model-failover

go 1.23

require github.com/wso2/api-platform/sdk/core v0.0.0 // match oauth2-generator's exact pinned version/replace

// copy oauth2-generator's replace directive(s) verbatim if this repo pins sdk/core to a local path
```

`policy-definition.yaml`:

```yaml
name: model-failover
version: v0.1.0
description: |
  Transparently retries a failed request against an ordered list of
  fallback model+endpoint targets (same request/response shape — e.g.
  several OpenAI-compatible deployments). Each attempt injects the
  target's configured model name into the request body. A target that
  just failed is suspended for suspendDuration so the next request skips
  straight past it. Requires each target's upstreamDefinition to already
  be declared on the same API/LlmProvider spec.

parameters:
  type: object
  additionalProperties: false
  required:
    - models
    - statusCodes
  properties:
    models:
      type: array
      minItems: 2
      x-wso2-policy-advanced-param: false
      description: |
        Ordered fallback chain. Index 0 is the primary target; every
        subsequent entry is tried in order if all earlier ones fail.
      items:
        type: object
        additionalProperties: false
        required:
          - name
          - upstreamDefinition
        properties:
          name:
            type: string
            minLength: 1
            description: Model name injected into the request body for this target.
          upstreamDefinition:
            type: string
            minLength: 1
            description: Must match an upstreamDefinitions[].name declared on the same API.
    statusCodes:
      type: array
      minItems: 1
      x-wso2-policy-advanced-param: false
      description: Response status codes that trigger failover to the next target.
      items:
        type: integer
        minimum: 100
        maximum: 599
    requestTimeout:
      type: string
      x-wso2-policy-advanced-param: true
      description: Per-attempt timeout (e.g. "10s"). Defaults to the route's own timeout when unset.
    suspendDuration:
      type: string
      x-wso2-policy-advanced-param: true
      description: |
        How long a failed target is skipped by future requests (e.g.
        "30s"). Omit to disable suspend tracking entirely.
    cache:
      type: object
      x-wso2-policy-advanced-param: true
      additionalProperties: false
      description: Only consulted when suspendDuration is set.
      properties:
        strategy:
          type: string
          enum: [memory, redis]
          default: memory
```

`model_failover.go` — copy this repo's existing `oauth2_generator.go` conventions for `getStringParam`/`getIntParam`/`getDurationParam`-style helpers verbatim (do not reinvent them; they already handle the `map[string]interface{}` decoding edge cases like YAML-vs-JSON numeric types) and write:

```go
package modelfailover

import (
	"fmt"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

type modelTarget struct {
	name               string
	upstreamDefinition string
}

type Policy struct {
	models          []modelTarget
	statusCodes     map[int]struct{}
	requestTimeout  time.Duration
	suspendDuration time.Duration // zero = suspend tracking disabled
	cacheStrategy   string
}

func GetPolicy(metadata policy.PolicyMetadata, params map[string]interface{}) (policy.Policy, error) {
	rawModels, ok := params["models"].([]interface{})
	if !ok || len(rawModels) < 2 {
		return nil, fmt.Errorf("model-failover requires at least 2 entries in 'models', got %d", len(rawModels))
	}
	models := make([]modelTarget, 0, len(rawModels))
	for i, raw := range rawModels {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("model-failover: models[%d] is not an object", i)
		}
		name, _ := m["name"].(string)
		def, _ := m["upstreamDefinition"].(string)
		if name == "" || def == "" {
			return nil, fmt.Errorf("model-failover: models[%d] requires both name and upstreamDefinition", i)
		}
		models = append(models, modelTarget{name: name, upstreamDefinition: def})
	}

	rawCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawCodes) == 0 {
		return nil, fmt.Errorf("model-failover requires a non-empty 'statusCodes' list")
	}
	statusCodes := make(map[int]struct{}, len(rawCodes))
	for _, raw := range rawCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok {
				code = int(f)
			} else {
				return nil, fmt.Errorf("model-failover: statusCodes entries must be integers")
			}
		}
		statusCodes[code] = struct{}{}
	}

	p := &Policy{models: models, statusCodes: statusCodes, cacheStrategy: "memory"}

	if raw := getStringParam(params, "requestTimeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid requestTimeout %q: %w", raw, err)
		}
		p.requestTimeout = d
	}
	if raw := getStringParam(params, "suspendDuration"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid suspendDuration %q: %w", raw, err)
		}
		p.suspendDuration = d
	}
	if cache, ok := params["cache"].(map[string]interface{}); ok {
		if strategy := getStringParam(cache, "strategy"); strategy != "" {
			p.cacheStrategy = strategy
		}
	}

	return p, nil
}

// Mode: needs the request body buffered (OnRequestBody, Task 8) to rewrite
// "model" and decide on a suspend-driven UpstreamName redirect; needs
// response headers (OnResponseHeaders, Task 10) to read the final
// x-envoy-attempt-count and record suspend state. Never needs response body
// or request headers alone. Mirrors oauth2-generator's own Mode()
// (oauth2_generator.go:641-651), which returns the same ProcessingMode
// struct shape with different phase selections for its own needs.
func (p *Policy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeProcess,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}
```

Confirm `getStringParam` and `policy.ProcessingMode`'s exact constant name by reading `oauth2_generator.go`'s own `getStringParam` (already read earlier this session, `oauth2_generator.go:671`) and `Mode()` (`oauth2_generator.go:641`) — copy verbatim, adjusting only for what this policy needs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd policies/model-failover && GOWORK=off go test ./... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd policies/model-failover  # inside the gateway-controllers checkout
git add go.mod policy-definition.yaml model_failover.go model_failover_test.go
git commit -m "feat(model-failover): scaffold policy config parsing"
```

### Task 8: Implement `OnRequestBody` — primary target injection and suspend-based skip-ahead

**Files:**
- Modify (gateway-controllers checkout): `policies/model-failover/model_failover.go`
- Test: `policies/model-failover/model_failover_test.go`

**Interfaces:**
- Consumes: `policy.RequestContext`/`RequestAction`/`UpstreamRequestModifications` (existing SDK types — `UpstreamRequestModifications.Body []byte`, `.UpstreamName *string`, both already exist per `sdk/core/policy/v1alpha2/action.go:127-146`); a new small `suspendStore` interface this task defines, backed by `sdk/core/utils/redisclient` when `cacheStrategy == "redis"` (mirror `oauth2-generator/token_cache.go`'s `cacheParams`/`extractRedisOverride`/`redisclient.Resolve` pattern exactly) or an in-memory map with expiry otherwise.
- Produces: `func (p *Policy) OnRequestBody(ctx context.Context, rctx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction`; `func (p *Policy) firstAvailableTarget(ctx context.Context) (index int, target modelTarget)`.

- [ ] **Step 1: Write the failing test**

```go
func TestOnRequestBody_NoSuspendUsesModelsZero(t *testing.T) {
	p := &Policy{
		models:      []modelTarget{{name: "gpt-4o", upstreamDefinition: "primary"}, {name: "gpt-4o-mini", upstreamDefinition: "fallback-1"}},
		statusCodes: map[int]struct{}{500: {}},
		suspend:     newMemorySuspendStore(), // add this small constructor in the same package — see Step 3
	}
	rctx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{},
		Body:          &policy.Body{Content: []byte(`{"model":"whatever-the-client-sent","messages":[]}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), rctx, nil)
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamRequestModifications, got %T", action)
	}
	if mods.UpstreamName != nil {
		t.Errorf("expected no UpstreamName override when nothing is suspended, got %q", *mods.UpstreamName)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(mods.Body, &decoded); err != nil {
		t.Fatalf("mutated body is not valid JSON: %v", err)
	}
	if decoded["model"] != "gpt-4o" {
		t.Errorf("expected model rewritten to the primary target's name, got %v", decoded["model"])
	}
}

func TestOnRequestBody_SuspendedPrimarySkipsAhead(t *testing.T) {
	p := &Policy{
		models:      []modelTarget{{name: "gpt-4o", upstreamDefinition: "primary"}, {name: "gpt-4o-mini", upstreamDefinition: "fallback-1"}},
		statusCodes: map[int]struct{}{500: {}},
		suspend:     newMemorySuspendStore(),
	}
	p.suspend.Suspend(context.Background(), suspendKey(&policy.SharedContext{APIId: "api-1", OperationPath: "/chat/completions"}, 0), time.Minute)

	rctx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{APIId: "api-1", OperationPath: "/chat/completions"},
		Body:          &policy.Body{Content: []byte(`{"model":"x","messages":[]}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), rctx, nil)
	mods := action.(policy.UpstreamRequestModifications)
	if mods.UpstreamName == nil || *mods.UpstreamName != "fallback-1" {
		t.Fatalf("expected UpstreamName override to the first non-suspended target, got %v", mods.UpstreamName)
	}
	var decoded map[string]interface{}
	json.Unmarshal(mods.Body, &decoded)
	if decoded["model"] != "gpt-4o-mini" {
		t.Errorf("expected model rewritten to the skipped-to target's name, got %v", decoded["model"])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run TestOnRequestBody -v`
Expected: FAIL — `OnRequestBody`/`suspend`/`newMemorySuspendStore`/`suspendKey` don't exist yet.

- [ ] **Step 3: Implement.** Add a `suspend` field to `Policy` (set in `GetPolicy`, defaulting to an in-memory store; a Redis-backed one is added in Task 10 once the cache-config plumbing is needed for real, following `oauth2-generator/token_cache.go`'s exact `extractCacheParams`/`redisclient.Resolve` pattern — do not build the Redis path in this task, keep it in-memory-only here and extend in Task 10):

```go
// suspendStore tracks which (route, target-index) pairs recently failed.
// The in-memory implementation here is intentionally the ONLY implementation
// this task adds — a Redis-backed one (for cross-replica suspend sharing) is
// added in Task 10, following oauth2-generator/token_cache.go's cache
// pattern; this interface is what makes that swap possible without touching
// OnRequestBody/OnResponseHeaders again.
type suspendStore interface {
	IsSuspended(ctx context.Context, key string) bool
	Suspend(ctx context.Context, key string, ttl time.Duration)
}

type memorySuspendStore struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry
}

func newMemorySuspendStore() *memorySuspendStore {
	return &memorySuspendStore{entries: make(map[string]time.Time)}
}

func (s *memorySuspendStore) IsSuspended(_ context.Context, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.entries[key]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.entries, key)
		return false
	}
	return true
}

func (s *memorySuspendStore) Suspend(_ context.Context, key string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = time.Now().Add(ttl)
}

// suspendKey scopes suspend state to this specific route/operation and
// target index — two different operations using model-failover must never
// share suspend state, and suspending target 0 must never affect target 1's
// own independent state.
func suspendKey(shared *policy.SharedContext, targetIndex int) string {
	return fmt.Sprintf("model-failover:%s:%s:%d", shared.APIId, shared.OperationPath, targetIndex)
}

// firstAvailableTarget walks p.models in order and returns the first whose
// suspend state (if suspendDuration is configured at all) isn't currently
// active. When suspendDuration is zero (disabled — see GetPolicy), or when
// every target is currently suspended (nothing better to do), it returns
// index 0 unconditionally — always trying the primary is strictly better
// than refusing to route at all.
func (p *Policy) firstAvailableTarget(ctx context.Context, shared *policy.SharedContext) (int, modelTarget) {
	if p.suspendDuration == 0 {
		return 0, p.models[0]
	}
	for i, m := range p.models {
		if !p.suspend.IsSuspended(ctx, suspendKey(shared, i)) {
			return i, m
		}
	}
	return 0, p.models[0]
}

// OnRequestBody runs once per client request, before anything is sent —
// this is the ONLY point that can redirect to a non-primary target ahead of
// time (the upstream-attempt phase, Task 9, can only react to a target
// Envoy already committed to dialing for that retry). It always rewrites
// "model" to the chosen target's configured name — this policy never
// passes through whatever model name the client sent, matching the old
// APIM UI's explicit "target model" selection semantics (see design spec).
func (p *Policy) OnRequestBody(ctx context.Context, rctx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	if rctx.Body == nil || !rctx.Body.Present {
		return policy.UpstreamRequestModifications{}
	}

	idx, target := p.firstAvailableTarget(ctx, rctx.SharedContext)

	var decoded map[string]interface{}
	if err := json.Unmarshal(rctx.Body.Content, &decoded); err != nil {
		slog.WarnContext(ctx, "ModelFailover: request body is not valid JSON, failing open (no mutation)", "error", err)
		return policy.UpstreamRequestModifications{}
	}
	decoded["model"] = target.name
	mutated, err := json.Marshal(decoded)
	if err != nil {
		slog.WarnContext(ctx, "ModelFailover: failed to re-marshal request body, failing open", "error", err)
		return policy.UpstreamRequestModifications{}
	}

	mods := policy.UpstreamRequestModifications{Body: mutated}
	if idx != 0 {
		name := target.upstreamDefinition
		mods.UpstreamName = &name
	}
	return mods
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run TestOnRequestBody -v`
Expected: PASS.

- [ ] **Step 5: Wire `suspend` into `GetPolicy`'s construction**

```go
// in GetPolicy, after parsing p.suspendDuration:
p.suspend = newMemorySuspendStore()
```

- [ ] **Step 6: Run the full package test suite**

Run: `cd policies/model-failover && GOWORK=off go build ./... && GOWORK=off go test ./... -v`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
cd policies/model-failover
git add model_failover.go model_failover_test.go
git commit -m "feat(model-failover): implement OnRequestBody with suspend-aware target selection"
```

### Task 9: Implement `OnUpstreamAttemptRequestHeaders` — per-attempt model rewrite

**Files:**
- Modify (gateway-controllers checkout): `policies/model-failover/model_failover.go`
- Test: `policies/model-failover/model_failover_test.go`

**Interfaces:**
- Consumes: `policy.UpstreamAttemptContext{AttemptCount, Body}`/`UpstreamAttemptHeaderModifications{Body}` (Task 2).
- Produces: `func (p *Policy) OnUpstreamAttemptRequestHeaders(ctx context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction`.

- [ ] **Step 1: Write the failing test**

```go
func TestOnUpstreamAttemptRequestHeaders_RewritesModelPerAttempt(t *testing.T) {
	p := &Policy{models: []modelTarget{{name: "gpt-4o", upstreamDefinition: "primary"}, {name: "gpt-4o-mini", upstreamDefinition: "fallback-1"}}}

	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Body:         &policy.Body{Content: []byte(`{"model":"gpt-4o","messages":[]}`), Present: true},
	}
	action := p.OnUpstreamAttemptRequestHeaders(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
	if !ok || mods.Body == nil {
		t.Fatalf("expected a body mutation, got %#v", action)
	}
	var decoded map[string]interface{}
	json.Unmarshal(mods.Body, &decoded)
	if decoded["model"] != "gpt-4o-mini" {
		t.Errorf("expected attempt 2 to inject models[1].name, got %v", decoded["model"])
	}
}

func TestOnUpstreamAttemptRequestHeaders_AttemptBeyondModelsListFailsOpen(t *testing.T) {
	p := &Policy{models: []modelTarget{{name: "a", upstreamDefinition: "x"}, {name: "b", upstreamDefinition: "y"}}}
	actx := &policy.UpstreamAttemptContext{AttemptCount: 5, Body: &policy.Body{Content: []byte(`{}`), Present: true}}
	action := p.OnUpstreamAttemptRequestHeaders(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
	if !ok || mods.Body != nil {
		t.Errorf("expected a no-op (nil Body) when AttemptCount exceeds len(models), got %#v", action)
	}
}

func TestOnUpstreamAttemptRequestHeaders_NilBodyFailsOpen(t *testing.T) {
	p := &Policy{models: []modelTarget{{name: "a", upstreamDefinition: "x"}, {name: "b", upstreamDefinition: "y"}}}
	actx := &policy.UpstreamAttemptContext{AttemptCount: 1, Body: nil} // cluster wasn't body-buffered for some reason
	action := p.OnUpstreamAttemptRequestHeaders(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
	if !ok || mods.Body != nil {
		t.Errorf("expected a no-op when actx.Body is nil, got %#v", action)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run TestOnUpstreamAttemptRequestHeaders -v`
Expected: FAIL with `(*Policy).OnUpstreamAttemptRequestHeaders` undefined.

- [ ] **Step 3: Implement.**

```go
// OnUpstreamAttemptRequestHeaders implements policy.UpstreamAttemptPolicy —
// AttemptCount N corresponds directly to p.models[N-1] (verified sufficient
// in the design spec: the translator builds the aggregate cluster's member
// list in this exact order, so no additional per-attempt target-identity
// plumbing — e.g. xds.cluster_name — is needed or reliable). Fails open
// (nil Body, no mutation) if AttemptCount is out of range or actx.Body is
// unavailable — this must only ever help a retry succeed, never add a new
// failure mode.
func (p *Policy) OnUpstreamAttemptRequestHeaders(ctx context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	if actx.Body == nil || !actx.Body.Present {
		return policy.UpstreamAttemptHeaderModifications{}
	}
	idx := actx.AttemptCount - 1
	if idx < 0 || idx >= len(p.models) {
		return policy.UpstreamAttemptHeaderModifications{}
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(actx.Body.Content, &decoded); err != nil {
		slog.WarnContext(ctx, "ModelFailover: upstream-attempt body is not valid JSON, failing open", "attempt", actx.AttemptCount, "error", err)
		return policy.UpstreamAttemptHeaderModifications{}
	}
	decoded["model"] = p.models[idx].name
	mutated, err := json.Marshal(decoded)
	if err != nil {
		slog.WarnContext(ctx, "ModelFailover: failed to re-marshal upstream-attempt body, failing open", "attempt", actx.AttemptCount, "error", err)
		return policy.UpstreamAttemptHeaderModifications{}
	}

	return policy.UpstreamAttemptHeaderModifications{Body: mutated}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run TestOnUpstreamAttemptRequestHeaders -v`
Expected: PASS.

- [ ] **Step 5: Run the full package test suite**

Run: `cd policies/model-failover && GOWORK=off go build ./... && GOWORK=off go test ./... -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd policies/model-failover
git add model_failover.go model_failover_test.go
git commit -m "feat(model-failover): rewrite model per upstream-attempt via AttemptCount indexing"
```

### Task 10: Implement `OnResponseHeaders` — record suspend state, add Redis-backed cache option

**Files:**
- Modify (gateway-controllers checkout): `policies/model-failover/model_failover.go`
- Create (gateway-controllers checkout): `policies/model-failover/redis_suspend_store.go`
- Test: `policies/model-failover/model_failover_test.go`, `policies/model-failover/redis_suspend_store_test.go`

**Interfaces:**
- Consumes: `policy.ResponseHeaderContext{ResponseHeaders, SharedContext}` (existing); `sdk/core/utils/redisclient.Resolve(opts *redis.Options, pingTimeout time.Duration) (*redis.Client, error)` (existing, `sdk/core/utils/redisclient/redisclient.go:212`).
- Produces: `func (p *Policy) OnResponseHeaders(ctx context.Context, rhctx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction`; `type redisSuspendStore struct{...}` implementing `suspendStore` (Task 8).

- [ ] **Step 1: Write the failing test**

```go
func TestOnResponseHeaders_FinalAttemptCountTwoSuspendsTargetZero(t *testing.T) {
	store := newMemorySuspendStore()
	p := &Policy{
		models:          []modelTarget{{name: "a", upstreamDefinition: "primary"}, {name: "b", upstreamDefinition: "fallback-1"}},
		suspend:         store,
		suspendDuration: time.Minute,
	}
	shared := &policy.SharedContext{APIId: "api-1", OperationPath: "/chat/completions"}
	rhctx := &policy.ResponseHeaderContext{
		SharedContext:   shared,
		ResponseHeaders: policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"2"}}),
	}

	p.OnResponseHeaders(context.Background(), rhctx, nil)

	if !store.IsSuspended(context.Background(), suspendKey(shared, 0)) {
		t.Error("expected target index 0 to be suspended after a final attempt count of 2 (it must have failed to trigger attempt 2)")
	}
	if store.IsSuspended(context.Background(), suspendKey(shared, 1)) {
		t.Error("target index 1 (the one that actually responded) must not be marked suspended")
	}
}

func TestOnResponseHeaders_AttemptCountOneSuspendsNothing(t *testing.T) {
	store := newMemorySuspendStore()
	p := &Policy{models: []modelTarget{{name: "a", upstreamDefinition: "primary"}, {name: "b", upstreamDefinition: "fallback-1"}}, suspend: store, suspendDuration: time.Minute}
	shared := &policy.SharedContext{APIId: "api-1", OperationPath: "/chat/completions"}
	rhctx := &policy.ResponseHeaderContext{SharedContext: shared, ResponseHeaders: policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"1"}})}

	p.OnResponseHeaders(context.Background(), rhctx, nil)

	if store.IsSuspended(context.Background(), suspendKey(shared, 0)) {
		t.Error("a first-attempt success must not suspend the primary")
	}
}

func TestOnResponseHeaders_SuspendDisabledIsNoOp(t *testing.T) {
	store := newMemorySuspendStore()
	p := &Policy{models: []modelTarget{{name: "a", upstreamDefinition: "primary"}, {name: "b", upstreamDefinition: "fallback-1"}}, suspend: store, suspendDuration: 0}
	shared := &policy.SharedContext{APIId: "api-1", OperationPath: "/chat/completions"}
	rhctx := &policy.ResponseHeaderContext{SharedContext: shared, ResponseHeaders: policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"2"}})}

	p.OnResponseHeaders(context.Background(), rhctx, nil)

	if store.IsSuspended(context.Background(), suspendKey(shared, 0)) {
		t.Error("suspendDuration == 0 must disable suspend tracking entirely, even with a multi-attempt response")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run TestOnResponseHeaders -v`
Expected: FAIL — `OnResponseHeaders` undefined.

- [ ] **Step 3a: Implement `OnResponseHeaders`.**

```go
// OnResponseHeaders infers which targets failed this request from the FINAL
// response's x-envoy-attempt-count: a final count of N means targets
// [0, N-2] all failed (each had to fail to trigger the next retry) — no new
// upstream-attempt response hook is needed for this (see design spec). A
// missing/unparseable header is treated as 1 (nothing failed), matching the
// same fail-toward-"first attempt" convention as the upstream-attempt phase.
// A no-op entirely when suspendDuration is 0 (suspend tracking disabled).
func (p *Policy) OnResponseHeaders(ctx context.Context, rhctx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if p.suspendDuration == 0 {
		return policy.DownstreamResponseHeaderModifications{}
	}

	finalAttemptCount := 1
	if vals := rhctx.ResponseHeaders.Get("x-envoy-attempt-count"); vals != "" {
		if n, err := strconv.Atoi(vals); err == nil && n > 0 {
			finalAttemptCount = n
		}
	}

	for i := 0; i < finalAttemptCount-1 && i < len(p.models); i++ {
		p.suspend.Suspend(ctx, suspendKey(rhctx.SharedContext, i), p.suspendDuration)
	}

	return policy.DownstreamResponseHeaderModifications{}
}
```

Confirm `policy.Headers`'s exact single-value getter method name (`Get`, used above) by checking its existing usage elsewhere in `oauth2_generator.go`/the SDK — adjust if this repo's actual method is named differently (e.g. `GetFirst`).

- [ ] **Step 3b: Implement the Redis-backed `suspendStore`.** New file `redis_suspend_store.go`, mirroring `oauth2-generator/token_cache.go`'s `extractCacheParams`/`extractRedisOverride`/`redisclient.Resolve` pattern exactly (copy that file's `extractRedisOverride`/`buildRedisKey`/`contextWithOptionalTimeout` helpers verbatim rather than reimplementing them — same repo, same conventions):

```go
package modelfailover

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	redisclient "github.com/wso2/api-platform/sdk/core/utils/redisclient"
)

type redisSuspendStore struct {
	client *redis.Client
}

func newRedisSuspendStore(params map[string]interface{}) (*redisSuspendStore, error) {
	override := extractRedisOverride(params) // copy this helper from token_cache.go verbatim
	client, err := redisclient.Resolve(override, defaultRedisConnectionTimeout) // copy this constant from token_cache.go
	if err != nil {
		return nil, err
	}
	return &redisSuspendStore{client: client}, nil
}

func (s *redisSuspendStore) IsSuspended(ctx context.Context, key string) bool {
	n, err := s.client.Exists(ctx, "model-failover:suspend:"+key).Result()
	if err != nil {
		return false // fail open — a Redis error must never make this feature block a request
	}
	return n > 0
}

func (s *redisSuspendStore) Suspend(ctx context.Context, key string, ttl time.Duration) {
	_ = s.client.Set(ctx, "model-failover:suspend:"+key, "1", ttl).Err() // best-effort; a failed write just means this replica won't see the suspension
}
```

- [ ] **Step 3c: Wire `cacheStrategy` into `GetPolicy`.** Replace the `p.suspend = newMemorySuspendStore()` line from Task 8 Step 5:

```go
if p.cacheStrategy == "redis" {
	store, err := newRedisSuspendStore(params)
	if err != nil {
		return nil, fmt.Errorf("model-failover: cacheStrategy 'redis' requires either a policy-level redis override or a gateway-level \"redis\" config section: %w", err)
	}
	p.suspend = store
} else {
	p.suspend = newMemorySuspendStore()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd policies/model-failover && GOWORK=off go test ./... -run 'TestOnResponseHeaders' -v`
Expected: PASS.

- [ ] **Step 5: Run the full package test suite**

Run: `cd policies/model-failover && GOWORK=off go build ./... && GOWORK=off go test ./... -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd policies/model-failover
git add model_failover.go redis_suspend_store.go model_failover_test.go redis_suspend_store_test.go
git commit -m "feat(model-failover): record suspend state from final attempt count, add Redis-backed store option"
```

### Task 11: Copy to `dev-policies/model-failover`, diff, dual commit

**Files:**
- Create (this repo): `gateway/dev-policies/model-failover/go.mod`, `policy-definition.yaml`, `model_failover.go`, `model_failover_test.go`, `redis_suspend_store.go`, `redis_suspend_store_test.go`

**Interfaces:**
- Consumes: everything from Tasks 7-10, byte-identical.
- Produces: a working local copy `gateway-runtime`'s builder can actually pick up (per `build.yaml`'s `filePath: ./dev-policies/model-failover` convention, matching `oauth2-generator`'s entry).

- [ ] **Step 1: Copy the files**

```bash
mkdir -p gateway/dev-policies/model-failover
cp <gateway-controllers-checkout>/policies/model-failover/go.mod gateway/dev-policies/model-failover/
cp <gateway-controllers-checkout>/policies/model-failover/policy-definition.yaml gateway/dev-policies/model-failover/
cp <gateway-controllers-checkout>/policies/model-failover/model_failover.go gateway/dev-policies/model-failover/
cp <gateway-controllers-checkout>/policies/model-failover/model_failover_test.go gateway/dev-policies/model-failover/
cp <gateway-controllers-checkout>/policies/model-failover/redis_suspend_store.go gateway/dev-policies/model-failover/
cp <gateway-controllers-checkout>/policies/model-failover/redis_suspend_store_test.go gateway/dev-policies/model-failover/
```

If `go.mod` in the `gateway-controllers` checkout has no local `replace` for `sdk/core` (it shouldn't — `sdk/core` is consumed as a real published dependency there, per `gateway-go-work-masks-sdk-core-replace-gap`), add a temporary local one in the `dev-policies` copy only, so this repo's `go.work` can build it against the in-progress SDK changes from Task 2:

```
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
```
marked `// TODO(model-failover): temporary local replace until sdk/core is tagged and go.mod bumped`, matching this repo's established convention for exactly this situation.

- [ ] **Step 2: Add `build.yaml` entry**

In `gateway/build.yaml`, add (alphabetically among the existing `policies:` entries):

```yaml
  - name: model-failover
    filePath: ./dev-policies/model-failover
```

- [ ] **Step 3: Diff to confirm byte-identical (aside from the temporary replace directive)**

```bash
diff <gateway-controllers-checkout>/policies/model-failover/model_failover.go gateway/dev-policies/model-failover/model_failover.go
diff <gateway-controllers-checkout>/policies/model-failover/policy-definition.yaml gateway/dev-policies/model-failover/policy-definition.yaml
diff <gateway-controllers-checkout>/policies/model-failover/redis_suspend_store.go gateway/dev-policies/model-failover/redis_suspend_store.go
```
Expected: no output (identical) for all three.

- [ ] **Step 4: Build and test the local copy**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go build ./... && GOWORK=off go test ./... -v`
Expected: all PASS.

- [ ] **Step 5: Commit (both repos)**

```bash
# api-platform (dev-policies mirror + build.yaml registration)
cd gateway
git add dev-policies/model-failover/ build.yaml
git commit -m "feat(model-failover): mirror policy from gateway-controllers into dev-policies"

# gateway-controllers is already committed per Tasks 7-10 — no further action there
```

---

## Phase 5: End-to-end verification

### Task 12: E2E test proving cross-cluster failover, per-attempt model rewrite, and suspend skip-ahead

**Files:**
- Create: `gateway/dev-policies/model-failover/e2e/mocks/mock-model-backend/main.go` (two instances run side-by-side on different ports via `ADDR`, mirroring `oauth2-generator/e2e/mocks/mock-ai-backend`'s exact structure/conventions — `/debug/force-status`, `/debug/reset`, `/debug/last-request`, catch-all echoing the received body's `model` field and the `Authorization` header)
- Create: `gateway/dev-policies/model-failover/e2e/postman/model-failover.postman_collection.json`
- Create: `gateway/dev-policies/model-failover/e2e/run-e2e.sh` (mirror `oauth2-generator/e2e/run-e2e.sh`'s structure: stack-up checks, mock startup, LlmProvider registration with `upstreamDefinitions` + the `model-failover` policy, retry loop, mock teardown)

**Interfaces:**
- Consumes: everything from Tasks 1-11; the exact mock-server conventions already established in `dev-policies/oauth2-generator/e2e/mocks/mock-ai-backend/main.go` (copy its `/debug/force-status`/`/debug/reset`/`handleAny` shape, changing only the response body to echo `model` instead of `Authorization`).

- [ ] **Step 1: Write the mock backend.** Copy `dev-policies/oauth2-generator/e2e/mocks/mock-ai-backend/main.go` to `dev-policies/model-failover/e2e/mocks/mock-model-backend/main.go`, and change `handleAny`'s response body construction to:

```go
var reqBody map[string]interface{}
_ = json.Unmarshal(body, &reqBody)
content := fmt.Sprintf("received model: %q", reqBody["model"])
```
(everything else — `/debug/force-status`, `/debug/reset`, `/debug/last-request`, the OpenAI-chat-completion-shaped response envelope — unchanged from the copied file).

- [ ] **Step 2: Write the Postman collection.** One folder, "Model failover", with:
1. `Reset backend A` / `Reset backend B` (`POST {{backendABaseUrl}}/debug/reset`, `POST {{backendBBaseUrl}}/debug/reset`).
2. `Register: model-failover-test` — an LlmProvider with:
```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: model-failover-test
spec:
  displayName: Model Failover Test
  version: v1.0
  template: openai
  context: /model-failover-test/latest
  upstream:
    url: http://host.docker.internal:9701
  upstreamDefinitions:
    - name: primary
      upstreams:
        - url: http://host.docker.internal:9701
    - name: fallback-1
      upstreams:
        - url: http://host.docker.internal:9702
  policies:
    - name: model-failover
      policyParams:
        models:
          - name: gpt-4o
            upstreamDefinition: primary
          - name: gpt-4o-mini
            upstreamDefinition: fallback-1
        statusCodes: [500]
        requestTimeout: 5s
        suspendDuration: 10s
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
```
3. `Force backend A to fail` (`POST {{backendABaseUrl}}/debug/force-status?code=500`).
4. `Invoke — expect clean 200 via fallback` — POST to `{{gatewayBaseUrl}}/model-failover-test/latest/chat/completions`; test script asserts `pm.response.to.have.status(200)` and `pm.expect(json.choices[0].message.content).to.include('received model: "gpt-4o-mini"')` (backend B's echoed model name proves the retry landed on the fallback with the correctly rewritten body, not just that *some* 200 came back).
5. `Verify backend A never saw a retry with the fallback model` — `GET {{backendABaseUrl}}/debug/last-request`; assert the logged body's `model` field is `"gpt-4o"` (backend A only ever saw the primary's own model name on its one attempt, proving the rewrite is genuinely per-attempt, not a global overwrite).
6. `Force backend A to fail again` + `Invoke again — expect UpstreamName skip-ahead straight to fallback` — same force-status + invoke, but this time assert via `GET {{backendABaseUrl}}/debug/last-request`'s `time` field (or a reset+recheck of `/debug/reset`'s effect) that backend A was **not** dialed at all this time — the suspend window from the first failure (Step 4's implicit suspend) should have caused `OnRequestBody` to route straight to backend B via `UpstreamName`, skipping backend A's attempt entirely. Reset backend A's forced-failure flag is irrelevant here since it should never be reached.
7. `Delete: model-failover-test`.

- [ ] **Step 3: Write `run-e2e.sh`.** Copy `dev-policies/oauth2-generator/e2e/run-e2e.sh`'s top-level structure (stack-reachability checks, starting the two mock backends via `go run .` on distinct `ADDR` values, the registration-wait-for-xDS-propagation pattern, the retry-the-whole-cycle-on-propagation-lag loop, teardown of only the mocks this script started) verbatim, pointed at this feature's collection and mocks.

- [ ] **Step 4: Run it**

Run: `cd gateway/dev-policies/model-failover/e2e && ./run-e2e.sh`
Expected: all Postman assertions pass. If the suspend/skip-ahead assertion in Step 2.6 fails specifically, re-check Task 8's `firstAvailableTarget`/`suspendKey` scoping before assuming it's a propagation-timing flake — a suspend bug is a much more likely cause than xDS timing for that specific assertion, since it doesn't depend on xDS propagation at all (it's a policy-level Redis/memory read, not a routing change).

- [ ] **Step 5: Commit**

```bash
cd gateway
git add dev-policies/model-failover/e2e/
git commit -m "test(model-failover): add e2e suite proving cross-cluster failover, per-attempt model rewrite, and suspend skip-ahead"
```
