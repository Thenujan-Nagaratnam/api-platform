# Contract: Failover Exhaustion Response (client-facing)

Returned by the front hop whenever no target can serve the request:
- the plan was empty (every target suspended with no probe slot available), or
- the final attempt was an eligible failure, a per-attempt timeout, or ran past the end of the plan.

**Status**: `503 Service Unavailable`

**Headers**:
- `content-type: application/json`
- `x-request-id`

No internal `x-wso2-failover-*` headers are included.

**Body**: always byte-identical, in OpenAI error format:

```json
{
  "error": {
    "message": "All configured model targets are currently unavailable. Please retry later.",
    "type": "model_failover_exhausted",
    "param": null,
    "code": "all_targets_unavailable"
  }
}
```

**Guarantees (FR-018)**:
- The status and body do not vary with which failures occurred.
- No provider names, endpoints, credentials, or upstream error bodies appear in it.
- When all targets are suspended, it is returned without contacting any upstream.

**Not an exhaustion case**: a non-eligible response (for example `400`, or a `503` when 503 isn't configured) from any target is returned to the client as-is, with its original status restored (FR-008).
