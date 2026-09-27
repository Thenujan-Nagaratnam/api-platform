# Quickstart: Validate Provider-Mode Model Failover

Uses the same IT stack and mocks as [001 quickstart](../001-model-failover-policy/quickstart.md) (`mock-llm-openai-a` on 8091, with `seq:` modes).

| # | Setup | Action | Expected |
|---|---|---|---|
| 1 | Provider `pa` (openai template, upstream mock A) with targets [gpt-a1, gpt-a2]; mock A `ok` | POST `/pa/chat/completions` | 200; mock A 1 request, model `gpt-a1` |
| 2 | Mock A `seq:status:429,ok` | POST | 200; mock A 2 requests; last model `gpt-a2`; provider credential on both |
| 3 | Mock A `status:400` | POST | 400; mock A 1 request |
| 4 | Mock A `seq:reset,ok` | POST | 200 via second model (connection reset) |
| 5 | Provider targets [gpt-a1, gpt-a2]; mock A `status:503` | POST | 503 exhaustion body; mock A 2 requests |
| 6 | threshold 2, suspend 5s; mock A `seq:status:503,ok,status:503,ok,ok` | 3 requests | third request's first attempt uses gpt-a2 (gpt-a1 suspended) |
| 7 | Any failover | inspect mock A requests | no `x-wso2-failover-*` header reached the provider |
| 8 | Gemini provider, policy on `/models/*`, mock A `seq:status:429,ok` | POST `/models/client:generateContent?alt=sse` | 200; last path has `/models/gemini-2.5-flash:generateContent` and `alt=sse`. Same policy on a fixed-model path: register → 400 |
| 9 | Target `provider: other` | register | 400 |
| 10 | Proxy over provider `pa` with its own failover | POST via proxy, mock A `seq:status:429,ok` | 200; nested failover served by gpt-a2 |
| 11 | All 001 scenarios | re-run | still pass |
