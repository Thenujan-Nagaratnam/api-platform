# Quickstart: Validating Model-Keyed Chains

Prerequisites: the 001 IT stack (mock LLMs `openai-a`, `openai-b`, `anthropic`), gateway built with this branch.

| # | Setup | Action | Expected |
|---|---|---|---|
| 1 | Proxy, chains `gpt-4o`→[openai-b gpt-4o, anthropic claude] and `gpt-4.1`→[gpt-4.1-mini] | POST model `gpt-4o`, all healthy | 200 from openai-a, 1 request, model `gpt-4o` |
| 2 | Same; mock A `seq:status:429,ok` | POST `gpt-4o` | 200 from openai-b, model `gpt-4o` |
| 3 | Same; mock A `seq:status:429,ok` | POST `gpt-4.1` | 200 from openai-a, second request model `gpt-4.1-mini`; openai-b and anthropic 0 requests |
| 4 | Same; mock A `status:503` | POST `gpt-4o-mini` (no chain) | 503 from mock A as is, 1 request, model `gpt-4o-mini`, no exhaustion body |
| 5 | Same | POST a non-JSON body | passed through, 1 attempt |
| 6 | All of `gpt-4o`'s chain failing | POST `gpt-4o` | exhaustion response; `gpt-4.1` unaffected |
| 7 | round-robin [gpt-4o, gpt-4.1] then model-failover | two POSTs, mock A `seq:status:429,ok,status:429,ok` | first served by openai-b (`gpt-4o` chain), second by `gpt-4.1-mini` |
| 8 | round-robin after model-failover | register | 400 |
| 9 | `targets` param | register | 400 pointing to `chains` |
| 10 | Provider (openai template), chains `gpt-a1`→[gpt-a2] | POST `gpt-a1`, mock A `seq:status:429,ok` | second attempt `gpt-a2` |
| 11 | Gemini provider on `/models/*`, chain `gemini-2.5-pro`→[gemini-2.5-flash] | POST `/models/gemini-2.5-pro:generateContent`, mock A `seq:status:429,ok` | second path has `gemini-2.5-flash` |
| 12 | Same Gemini provider | POST `/models/gemini-1.5:generateContent` (no chain), mock A `status:429` | 429 as is, 1 request |
