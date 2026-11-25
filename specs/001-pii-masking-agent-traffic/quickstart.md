# Quickstart: Validating the Patch

Run from the `gateway-controllers` repository, `policies/pii-masking-regex`.

## 1. Unit tests

```bash
cd policies/pii-masking-regex
go test ./...
```

Expect all existing tests plus the new cases in `mask_test.go`, `jsonmask_test.go` and `restore_test.go` to pass.

## 2. Item checks (each must pass)

| Item | Check | Expected |
|---|---|---|
| 1 | Body with `"line1\nbob@example.org"`, `email: true`, `jsonPath: "$"` | Output is valid JSON, `\n` intact, email replaced. Repeat with `\t`, `\r` and `<`. |
| 2 | Tool schema with `"maximum": 9007199254740991`, `phone: true` | Number byte-identical, JSON valid. |
| 3 | Stream with `input_json_delta` holding a placeholder | Client receives the original value in `partial_json`. |
| 4 | Same body 20 times, `email: true`, `phone: true` | One distinct upstream body. |
| 5 | Stream with `thinking_delta` holding a placeholder | Client receives the original value. |
| 6 | One flush holding two text blocks | Each block keeps its own text, none empty. |
| 7 | Two instances (email, phone), streamed and not | Both kinds restored. |
| 8 | Scoped path, email in system prompt, tool description, user text, tool result | Only the last two masked. With `"$"`, all four masked. |
| 9 | `"started 1795104230191"`, `"id 2147483647"`, `"4155550199"` | Timestamp untouched. The two standalone 10-digit numbers still masked. |

## 3. Regression guard (the "keep today's masking" rule)

Replay a set of requests that were valid and masked on v1.1.0 and compare the set of masked values. Every value masked before must still be masked, apart from matches inside longer digit runs.

## 4. Integration suite

```bash
cd policies/pii-masking-regex/it
go test ./...
```

New scenarios in `features/pii_masking_regex.feature` cover items 1 to 9 against a mock Anthropic upstream.

## 5. Gateway

After the policy release, bump the module reference in the gateway patch, rebuild the gateway images yourself, and run the Messages scenario through the AI gateway with streaming, tools and thinking.
