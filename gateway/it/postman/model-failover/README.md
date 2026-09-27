# Model Failover E2E (Postman / newman)

End-to-end tests for the `model-failover` policy, run against the gateway integration-test stack. They complement the godog scenarios in `gateway/it/features/model-failover.feature` with a wider matrix: every eligible and non-eligible status, transport failures, timeouts, streaming, cross-provider conversion, suspension and recovery, exhaustion, updates and every validation rule.

## What it needs

The IT stack from `gateway/it/docker-compose.test.yaml`, built from this branch:

| Service | Host port | Used for |
|---|---|---|
| gateway-runtime (router) | 8080 | the proxies under test |
| gateway-controller REST API | 9090 | creating providers and proxies (`admin`/`admin`) |
| mock-llm-openai-a | 8091 | OpenAI-format target A |
| mock-llm-openai-b | 8092 | OpenAI-format target B |
| mock-llm-anthropic | 8093 | Anthropic-format target |

The mocks (`tests/mock-servers/mock-llm-provider`) are switched per request through `PUT /__mode` (`ok`, `status:<code>`, `hang:<s>`, `reset`, `stream-abort:<n>`, `seq:<mode>,<mode>…`) and inspected through `GET /__requests`.

## Run

```bash
cd gateway
make build-coverage                                   # runtime before controller
cd it
docker compose -f docker-compose.test.yaml up -d --build
./postman/model-failover/run-e2e.sh                   # whole suite, about 1-2 minutes
docker compose -f docker-compose.test.yaml down
```

Run one area with newman's `--folder`, always followed by cleanup:

```bash
./postman/model-failover/run-e2e.sh --folder "00 Setup" --folder "05 Cross-provider failover" --folder "99 Cleanup"
```

Every resource name carries a random run id (`mfe<run>-…`), so a failed run never collides with the next one. A JUnit report is written to `reports/`.

## Coverage

| Folder | Cases |
|---|---|
| 00 Setup | Mocks are up; four LlmProviders (two OpenAI, one on a closed port, one Anthropic) |
| 01 Ordered failover | Healthy primary (model rewrite, credential, internal headers hidden from the provider); 429, 500, 502, 503, 504 each fail over with the fallback's own credential and model; 400, 401, 403, 404, 422 pass through; connection reset; hanging primary abandoned at `perAttemptTimeout`; streaming request fails over before the stream starts; forged internal headers ignored; operations without the policy are not failed over; a 200 KB body is resent intact |
| 02 Status allow-list | `statusCodes: [429, 529]`: 503 passes through, 529 and 429 fail over |
| 03 Timeout not a failover condition | `failoverOn.timeout: false`: a slow target returns 504, no failover |
| 04 Connection failures | Closed port fails over; with `connectFailure: false` the 503 reaches the client |
| 05 Cross-provider failover | Anthropic fallback: `/v1/messages`, `x-api-key` only, `anthropic-version`, target model; OpenAI-shaped answer; streamed answer converted to an OpenAI stream |
| 06 Same provider, two models | An unlisted 529 passes through; a 503 on the first model fails over to the second model on the same provider |
| 07 Exhaustion | All targets fail (status mix, then transport + status mix): fixed 503 body, each target tried once |
| 08 Suspension and recovery | Suspend after N failures and skip; probe after `suspendDuration` recovers; a failed probe suspends again; every target suspended answers without any upstream call |
| 09 Global attachment and updates | `globalPolicies` attachment; a `PUT` that changes the chain's primary takes effect |
| 10 Configuration validation | 18 invalid configurations rejected with 400: the removed `targets`, duplicate primaries, a provider on a primary, bounds, and a provider-selecting policy on the same operation |
| 11 Provider-mode model failover | model-failover on an LlmProvider: first model serves (own credential, no internal headers); 429, reset and a hanging first model fall back to the second model; 400 passes through; exhaustion; suspension skips a failing model; a proxy over that provider (nested failover); a Gemini-template provider fails over by rewriting the model in the path; a path that fixes the model and cross-provider fallbacks are rejected |
| 12 Chains keyed by the requested model | Two chains on one proxy: `gpt-4.1` and `gpt-4o` each fall back along their own chain; an unconfigured model passes through with one attempt and its 503 unchanged; one chain exhausting; model-round-robin before model-failover picks the chain; round-robin after it is rejected |
| 99 Cleanup | Deletes every proxy and provider the run created |

Not covered here and why:
- **A failure after streaming has started.** The upstream cuts the connection mid-stream, which newman reports as a request error. The godog scenario "No failover happens once the response has started streaming" covers it.
- **Bodies larger than the retry buffer.** Pending task T050.
- **Metrics.** Pending the `sdk/core` tag (T053).

## Editing

Edit `generate_collection.py`, then regenerate:

```bash
python3 generate_collection.py
```

Don't edit the JSON by hand.
