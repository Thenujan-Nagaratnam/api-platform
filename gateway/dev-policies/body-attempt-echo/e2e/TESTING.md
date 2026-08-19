# body-attempt-echo — e2e testing

Dev-only policy (see `dev-policies/README.md`) that exists solely to prove, end
to end, gateway-controller's `x-wso2-upstream-attempt: {body: true}` opt-in
(commit `4204a943c` on `decoupled-retry-source`) — the capability that lets a
policy get `UpstreamAttemptContext.Body` populated on a **plain, same-endpoint**
`resilience.retry` route, not just on a retry-source/aggregate-cluster route
like model-failover's.

On every upstream dial attempt, `OnUpstreamAttemptRequest` rewrites the
outgoing JSON body's `"attempt"` field to Envoy's current
`x-envoy-attempt-count`. `mock-echo-backend` keeps a full request **history**
(not just the last request), so the test can distinguish attempt 1's original
body from attempt 2's mutated one.

## Quick start

```bash
cd gateway
make build
docker compose up -d gateway-controller gateway-runtime sample-backend redis
cd dev-policies/body-attempt-echo/e2e
./run-e2e.sh
```

This starts `mock-echo-backend` natively (`go run .`, port `:9720`), registers
the `body-attempt-echo-test` RestApi, waits for xDS propagation, runs both
flows below via `newman`, and cleans up.

## Flows

- **02 — Baseline**: a single request with no forced failure. Proves the
  mutation fires even on attempt 1, with no retry involved — `receivedAttempt`
  in the response is `1`.
- **03 — Retry**: `POST /debug/force-status?code=503` makes the first dial
  fail; body-attempt-echo's `x-wso2-retry-conditions` (`statusCodes: [503]`,
  `minAttempts: 2`) makes Envoy retry the same cluster; the retried attempt's
  body is rewritten to `attempt: 2`. The client only ever sees the final `200`.
  `GET /debug/history` on the mock proves both dials actually reached the
  backend, with different `attempt` values and the original `message` field
  intact on both.

## Manual verification

```bash
curl -sk -X POST https://localhost:8443/body-attempt-echo-test/v1.0/echo \
  -H 'Content-Type: application/json' -d '{"message":"hello"}' | jq

curl -s -X POST http://localhost:9720/debug/force-status?code=503
curl -sk -X POST https://localhost:8443/body-attempt-echo-test/v1.0/echo \
  -H 'Content-Type: application/json' -d '{"message":"hello"}' | jq
curl -s http://localhost:9720/debug/history | jq
```

## Cleanup

```bash
curl -X DELETE http://localhost:9090/api/management/v1/rest-apis/body-attempt-echo-test \
  -H 'Authorization: Basic YWRtaW46YWRtaW4='
```

Remember to remove `body-attempt-echo`'s `filePath` entry from `gateway/build.yaml`
before committing anything beyond this dev/test policy itself (per
`dev-policies/README.md`).
