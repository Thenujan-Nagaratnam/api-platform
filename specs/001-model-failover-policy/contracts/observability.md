# Contract: Logs and Metrics

## Metrics (policy-engine metrics endpoint, via the new `sdk/core/metrics` registerer)

Label cardinality is bounded:
- `chain`: one value per configured failover attachment.
- `target`: `t0`–`t9`, plus `provider` and `model` as configured.
- `reason`: from a closed set: `status_429`, `status_5xx_<code>`, `connect_failure`, `reset`, `timeout`.

| Metric | Type | Labels | Emitted when |
|---|---|---|---|
| `wso2_model_failover_attempts_total` | counter | chain, target, outcome (`success`/`non_eligible`/`eligible_failure`), probe | Every dispatch attempt |
| `wso2_model_failover_failovers_total` | counter | chain, from_target, reason | Each eligible failure followed by another attempt |
| `wso2_model_failover_served_total` | counter | chain, target, position | The final client response came from this target |
| `wso2_model_failover_exhausted_total` | counter | chain, cause (`all_suspended`/`all_failed`) | Exhaustion response returned |
| `wso2_model_failover_target_state` | gauge | chain, target | 0 = healthy, 1 = suspended, 2 = probing |
| `wso2_model_failover_state_transitions_total` | counter | chain, target, to | Every health state change |
| `wso2_model_failover_attempt_duration_seconds` | histogram | chain, target, outcome | Every dispatch attempt (time to response headers) |

Envoy's own stats on `failover_dispatch` (`upstream_rq_retry`, `_retry_success`, `_retry_overflow`, `_retry_limit_exceeded`) stay available for platform-level alerting.

## Structured logs (`slog`, gateway log pipeline)

| Event | Level | Fields |
|---|---|---|
| `model_failover.attempt_failed` | WARN | request_id, chain, target, provider, model, position, reason, latency_ms |
| `model_failover.served` | INFO (DEBUG when position = 0) | request_id, chain, target, position, attempts |
| `model_failover.exhausted` | WARN | request_id, chain, cause, attempted_targets |
| `model_failover.target_suspended` | WARN | chain, target, consecutive_failures, suspended_until |
| `model_failover.target_probing` / `model_failover.target_recovered` | INFO | chain, target, probe_successes |
| `model_failover.plan_rejected` | ERROR | request_id, chain, cause (`unknown_nonce`/`chain_mismatch`) |

**Never logged (FR-021)**: credentials, `Authorization` or API-key headers, the `x-wso2-failover-hop` secret, request or response bodies, or raw upstream error bodies.
