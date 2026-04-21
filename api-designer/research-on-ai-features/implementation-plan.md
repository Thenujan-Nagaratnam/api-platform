# AI Readiness Feature - Full Implementation Plan

## Current State Summary

The extension currently has:
- **4 Spectral rule categories**: summaries, descriptions, examples, error responses
- **Scoring**: Simple average of 4 category coverage percentages (0–100%)
- **UI**: `AIReadinessDashboard.tsx` with metric tiles, progress bars, and "Fix with AI" buttons that open Copilot chat
- **AI**: Multi-provider architecture exists; Copilot implemented, Claude provider is a stub returning `{ success: false }`
- **Data flow**: Spectral ruleset on GitHub → custom Spectral functions → metrics collector → `buildAiReadinessSummary()` → dashboard

### Key files to modify

| File | What it does |
|---|---|
| `api-designer-core/src/utils/ai-readiness.ts` | Score computation and summary building |
| `api-designer-extension/src/utils/ai-readiness-functions.ts` | Custom Spectral functions |
| `api-designer-core/src/constants/default-spectral-rulesets.ts` | Ruleset URLs |
| `api-designer-extension/src/rpc-managers/.../governance-manager.ts` | RPC handler |
| `api-designer-extension/src/utils/validation-utils.ts` | Spectral runner |
| `api-designer-visualizer/src/views/AnalyzeView/components/AIReadinessDashboard.tsx` | UI |
| `api-designer-core/src/rpc-types/.../analyze.ts` | All shared types |
| `api-designer-extension/src/ai/providers/claude-provider.ts` | Claude stub |

---

## Design Philosophy

Agents consume APIs differently from humans:
- Humans read docs sequentially and tolerate ambiguity via context
- Agents parse specs programmatically, pick endpoints by semantic match, and retry automatically
- Agents need machine-readable contracts: deterministic error shapes, idempotency signals, pagination contracts, unambiguous operation identities

This means AI readiness is not just documentation quality. It is runtime behavioral correctness for autonomous consumers.

The feature has **two engines** with different execution models:

- **Engine A (Static)**: Always-on Spectral-based evaluation. Deterministic, fast, inline diagnostics.
- **Engine B (LLM)**: On-demand evaluation triggered by user. Semantic, advisory, stored as a report file.

---

## Phase 1: Expand Static Engine (Engine A)

### 1.1 New scoring model

Replace the current flat average of 4 percentages with a weighted 100-point model:

| Bucket | Points | Replaces current |
|---|---|---|
| Specification Reliability | 30 | partial overlap with examples/error responses |
| Operation Semantics | 25 | replaces summaries + descriptions |
| Agent Execution Safety | 30 | new |
| Governance and Discoverability | 15 | new |

**Total static max: 100 points.** No normalization needed — the score is the score.

### 1.2 Scoring bands

| Score | Label |
|---|---|
| 0–39 | Not Ready |
| 40–59 | Basic |
| 60–74 | Improving |
| 75–89 | Ready |
| 90–100 | Agent Strong |

### 1.3 Rule breakdown per bucket

#### Bucket 1: Specification Reliability (30 pts)

What agents need: a structurally valid, fully-resolvable spec. Without this, any tool consuming the spec will fail before reaching meaningful operations.

| Rule ID | Check | Points | Severity |
|---|---|---|---|
| `spec-parse-valid` | Spec parses without YAML/JSON errors | 10 | Critical |
| `refs-resolvable` | All `$ref` values resolve without dead links | 8 | Critical |
| `no-contradictory-schema` | No allOf/oneOf/anyOf with contradictory required + type constraints | 6 | High |
| `response-schemas-present` | Every operation has at least one 2xx response schema | 6 | High |

**Why these matter for agents:** An agent calling a tool-use API with an unresolvable `$ref` will fail to construct the call schema. Contradictory constraints cause schema validation failures at runtime.

#### Bucket 2: Operation Semantics (25 pts)

What agents need: deterministic operation identity and enough natural language to map user intent to the correct endpoint.

| Rule ID | Check | Points | Severity |
|---|---|---|---|
| `operation-id-present` | All operations have `operationId` | 6 | High |
| `operation-id-unique` | All `operationId` values are unique across the spec | 6 | Critical |
| `summary-present` | All operations have a `summary` | 5 | High |
| `description-present` | All operations and path-level parameters have `description` | 4 | Medium |
| `param-type-format` | Path and query parameters define `type`, `format`, or `enum` | 4 | Medium |

**Why these matter for agents:** Agents use `operationId` as a stable tool name. Duplicate IDs cause call ambiguity. Missing summaries degrade intent-to-endpoint routing accuracy.

#### Bucket 3: Agent Execution Safety (30 pts)

What agents need: predictable runtime behavior for retries, pagination, and failure handling.

| Rule ID | Check | Points | Severity |
|---|---|---|---|
| `standard-error-body` | Error responses (4xx, 5xx) share a consistent schema shape | 10 | High |
| `rate-limit-response` | Operations that can return 429 include a `Retry-After` header schema | 7 | High |
| `idempotency-declaration` | POST/PUT/PATCH operations with retry risk declare idempotency key header or note | 6 | Medium |
| `pagination-support` | Collection endpoints (GET returning arrays) include pagination parameters | 5 | Medium |
| `correct-status-semantics` | CRUD operations use correct HTTP status codes (201 for POST create, 204 for DELETE, etc.) | 2 | Low |

**Why these matter for agents:** Agents retry failed calls. Without idempotency signals, a retry on a payment endpoint causes double-charges. Without consistent error shapes, the agent cannot parse failure reason and falls back to generic handling. Without `Retry-After`, agents spin-loop on rate-limited endpoints.

#### Bucket 4: Governance and Discoverability (15 pts)

What agents need: safe exposure, correct auth contracts, and metadata for discovery.

| Rule ID | Check | Points | Severity |
|---|---|---|---|
| `security-scheme-defined` | At least one security scheme defined in `components.securitySchemes` | 4 | Critical |
| `security-applied` | Security scheme applied globally or at each operation | 4 | High |
| `https-servers` | All server URLs use `https://` (not `http://`) | 3 | High |
| `no-secrets-in-examples` | No obvious secrets (API keys, passwords) in example values or defaults | 2 | Critical |
| `tags-and-description` | API-level `info.description` and at least one tag defined | 2 | Low |

**Why these matter for agents:** An agent operating autonomously must not accidentally call unauthenticated endpoints or leak secrets present in examples. Tags enable discovery filtering by category.

### 1.4 Implementation steps for Engine A

#### Step A1: Update `AiReadinessCategorySummary` type

File: `api-designer-core/src/rpc-types/api-designer-visualizer/analyze.ts`

Add a new `bucket` field to distinguish the four static buckets, and extend `AiReadinessSummary` to use the new 4-bucket structure instead of the current 4-field flat structure.

```ts
type StaticBucketId =
  | "specificationReliability"
  | "operationSemantics"
  | "agentExecutionSafety"
  | "governanceDiscoverability";

interface StaticBucketSummary {
  id: StaticBucketId;
  label: string;
  maxPoints: number;
  earnedPoints: number;
  percentage: number;
  findings: AiReadinessFinding[];
}

interface AiReadinessFinding {
  ruleId: string;
  severity: "critical" | "high" | "medium" | "low";
  message: string;
  impact: string;         // one-line "why this matters for agents"
  location?: string;      // JSONPath or path string
  suggestion?: string;    // optional static quick-fix hint
}

interface StaticReadinessResult {
  score: number;              // 0–100
  band: ReadinessBand;
  buckets: StaticBucketSummary[];
  topFindings: AiReadinessFinding[];  // top 5 by severity × points
}
```

#### Step A2: Rewrite Spectral custom functions

File: `api-designer-extension/src/utils/ai-readiness-functions.ts`

For each new rule, write or extend a Spectral custom function. The functions should:
1. Accept the resolved spec object
2. Traverse relevant nodes
3. Return violations in Spectral format with a `message` and `path`
4. Register pass/fail counts into `AiReadinessMetricsCollector` per bucket

Key new functions needed:
- `refsResolvableFunction`: Walk all `$ref` nodes and verify resolution
- `standardErrorBodyFunction`: Compare error response schemas for structural consistency (at minimum: `code`/`message` or `title`/`detail` fields present across all)
- `rateLimitResponseFunction`: Check operations with 429 responses for `Retry-After` header
- `idempotencyDeclarationFunction`: Check POST/PUT/PATCH for idempotency key header parameter or x-idempotent extension
- `paginationSupportFunction`: Check GET collection operations for `page`/`cursor`/`offset` parameters
- `noSecretsInExamplesFunction`: Pattern match example values against common secret patterns (e.g., `sk-`, `Bearer `, `password`)

#### Step A3: Update scoring engine

File: `api-designer-core/src/utils/ai-readiness.ts`

Replace `computeReadinessScoreFromMetrics()` with a points-based calculator:

```ts
function computeStaticScore(buckets: StaticBucketSummary[]): number {
  return buckets.reduce((sum, b) => sum + b.earnedPoints, 0); // max 100
}
```

Replace `buildAiReadinessSummary()` with `buildStaticReadinessResult()` that maps Spectral violations to the 4 buckets using the rule ID prefix convention (`spec-`, `op-`, `safety-`, `gov-`).

#### Step A4: Update `governance-manager.ts`

File: `api-designer-extension/src/rpc-managers/.../governance-manager.ts`

Update `calculateAIReadinessScore()` to call the new `buildStaticReadinessResult()` and return `StaticReadinessResult` instead of the current flat summary.

Add a new RPC method `getStaticReadiness(filePath)` that returns `StaticReadinessResult`. Keep `getGovernance()` for backward compatibility with the governance tab.

#### Step A5: Update `AIReadinessDashboard.tsx`

Replace the 4 metric tiles with 4 bucket cards. Each card shows:
- Bucket label (e.g., "Agent Execution Safety")
- Points earned / max (e.g., "18 / 25")
- Progress bar
- Expand to show per-rule findings with severity icons and one-line impact statements

Retain the existing "Fix with AI" pattern at the finding level.

---

## Phase 2: Add LLM Engine (Engine B)

### 2.1 Design principle: qualitative only, no score contribution

The LLM engine does **not** produce a score or adjust the static score. The static score (0–100) is the single authoritative metric.

**Why:** LLM evaluations are non-deterministic — the same spec could score differently across runs. Mixing a non-deterministic number into the score would undermine user trust in the metric. What LLM evaluation is actually good at is surfacing *specific, named problems* that static rules cannot detect: semantic confusion, intent ambiguity, missing workflow context, and operations that would predictably mislead an agent.

The LLM report is a separate **"AI Insights"** panel that sits alongside the score but never changes it. Users understand a score; they also understand "here are 3 operations that will confuse an agent, here's why, here's how to fix them."

### 2.2 What the LLM engine evaluates

Static rules cannot evaluate:
- Whether a summary is semantically accurate vs. just present
- Whether two endpoints are confusingly similar in purpose (misrouting risk)
- Whether multi-step workflows can be inferred from the spec
- Whether descriptions include practical execution guidance (preconditions, side effects, retry context)
- Whether field names are ambiguous or collide in meaning across operations

These require natural language reasoning. The LLM is asked to read the spec as an agent would — trying to figure out what to call and when — and report where it gets confused.

### 2.3 Four LLM insight categories

| Category | What it surfaces |
|---|---|
| **Intent Mapping** | Operations whose summaries/descriptions would cause an agent to pick the wrong endpoint for a user goal |
| **Ambiguity and Collision** | Confusingly similar endpoints, vague or overloaded field names, likely misrouting hotspots |
| **Workflow Reasoning** | Missing links between related operations — flows that can't be inferred without out-of-band knowledge |
| **Documentation Actionability** | Descriptions that lack preconditions, side effects, or retry context an agent needs to execute safely |

Each category produces a list of **findings** — not a number. Each finding is tied to a specific operation or field, includes the concrete problem, a suggested fix, and a confidence label.

### 2.4 Finding priority levels

Instead of scores, findings carry a **priority** that drives ordering in the UI:

| Priority | Meaning |
|---|---|
| `critical` | Would likely cause an agent to call the wrong endpoint or corrupt state |
| `warn` | Would degrade agent reliability or require fallback handling |
| `suggest` | Improvement that would meaningfully help agent accuracy |

Confidence (`High` / `Medium` / `Low`) is separate and reflects the LLM's certainty about the finding, not its severity.

### 2.5 LLM prompt design

The prompt instructs the LLM to reason as an autonomous agent would, not as a human reviewer.

**System prompt:**
```
You are evaluating an OpenAPI specification to identify problems that would cause
an autonomous AI agent to fail, misroute, or behave incorrectly when consuming this API.

You are NOT grading documentation quality for humans. You are identifying specific,
actionable problems an agent would encounter.

Return ONLY valid JSON matching the schema provided. Do not add commentary outside the JSON.
```

**User prompt template:**
```
Analyze this OpenAPI specification from the perspective of an autonomous AI agent
that will use it as a tool manifest.

Identify concrete problems across these four categories:
1. intentMapping — operations an agent would pick incorrectly for a given user goal
2. ambiguityCollision — endpoints or fields that are confusingly similar or overloaded
3. workflowReasoning — multi-step flows that cannot be inferred from the spec alone
4. documentationActionability — operations missing preconditions, side effects, or retry guidance

Spec:
<spec content>

Return a JSON object with this exact schema:
{
  "intentMapping": {
    "findings": [
      {
        "operationId": string,
        "problem": string,         // what specifically would confuse an agent
        "suggestion": string,      // concrete fix (rewritten summary, added note, etc.)
        "confidence": "High" | "Medium" | "Low",
        "priority": "critical" | "warn" | "suggest"
      }
    ]
  },
  "ambiguityCollision": {
    "findings": [{ "operationId": string, "problem": string, "suggestion": string, "confidence": string, "priority": string }]
  },
  "workflowReasoning": {
    "findings": [{ "operationId": string, "problem": string, "suggestion": string, "confidence": string, "priority": string }]
  },
  "documentationActionability": {
    "findings": [{ "operationId": string, "problem": string, "suggestion": string, "confidence": string, "priority": string }]
  },
  "summary": string   // 2–3 sentence plain English summary of the biggest agent risks in this spec
}

Only include findings where there is a real, specific problem. Return empty arrays for categories with no issues.
```

### 2.6 Claude provider implementation

File: `api-designer-extension/src/ai/providers/claude-provider.ts`

The current stub must be replaced with a real implementation:

```ts
import Anthropic from "@anthropic-ai/sdk";

export class ClaudeProvider implements AIProvider {
  private client: Anthropic;

  constructor() {
    const apiKey = vscode.workspace
      .getConfiguration("apiDesigner.ai.claude")
      .get<string>("apiKey");
    if (!apiKey) throw new Error("Claude API key not configured");
    this.client = new Anthropic({ apiKey });
  }

  async generateWithAI(context: AIContext, prompt: string): Promise<AIResponse> {
    try {
      const message = await this.client.messages.create({
        model: "claude-sonnet-4-6",
        max_tokens: 4096,
        system: context.systemPrompt,
        messages: [{ role: "user", content: prompt }],
      });
      return {
        success: true,
        content: message.content[0].type === "text" ? message.content[0].text : "",
      };
    } catch (e) {
      return { success: false, error: String(e) };
    }
  }
}
```

Add VS Code setting: `apiDesigner.ai.claude.apiKey` (type: string, secret, description: "Anthropic API key for LLM evaluation").

### 2.7 LLM Evaluation runner

Create a new file: `api-designer-extension/src/utils/llm-readiness-runner.ts`

Responsibilities:
1. Accept spec content string and file path
2. Compute `specHash` (SHA-256 of normalized content)
3. Build the LLM prompt (with spec truncation if needed — see Phase 4)
4. Call `AIManager.generateWithAI()`
5. Parse and validate the JSON response
6. Run acceptance checks (see 2.8)
7. Build the `LLMInsightsReport` object
8. Persist to `ai/readiness/` folder

### 2.8 Acceptance checks for LLM output

Before treating LLM output as valid:
- All 4 category `findings` arrays are present (can be empty)
- Each finding has `operationId`, `problem`, `suggestion`, `confidence`, `priority`
- `priority` values are one of: `critical`, `warn`, `suggest`
- `confidence` values are one of: `High`, `Medium`, `Low`
- `summary` field is a non-empty string
- JSON schema validates against expected shape

If checks fail: store the report with `acceptedByChecks: false` and display a warning in the UI instead of showing findings. Do not silently hide the failure.

### 2.9 Report persistence

#### Folder structure (created inside the workspace folder, not source code)

```
ai/
  readiness/
    index.json          ← lookup index: apiPath → latest reportId
    report.json         ← latest full report (for quick load)
    history/
      report-<id>.json  ← full report archive
```

#### Report JSON schema

```ts
interface LLMInsightsReport {
  reportId: string;              // uuid
  apiPath: string;               // workspace-relative path
  specHash: string;              // SHA-256 of normalized spec
  engineType: "llm";
  generatedAt: string;           // ISO 8601
  model: string;                 // e.g., "claude-sonnet-4-6"
  promptVersion: string;         // e.g., "v1.0"
  categories: {
    intentMapping: LLMCategoryResult;
    ambiguityCollision: LLMCategoryResult;
    workflowReasoning: LLMCategoryResult;
    documentationActionability: LLMCategoryResult;
  };
  summary: string;               // plain English summary of biggest agent risks
  acceptedByChecks: boolean;
}

interface LLMCategoryResult {
  findings: LLMFinding[];
}

interface LLMFinding {
  operationId?: string;
  problem: string;
  suggestion: string;
  confidence: "High" | "Medium" | "Low";
  priority: "critical" | "warn" | "suggest";
}
```

#### Index file structure

```ts
interface ReadinessIndex {
  entries: Array<{
    apiPath: string;
    latestReportId: string;
    specHash: string;
    generatedAt: string;
  }>;
}
```

#### Freshness logic

When a file is opened:
1. Compute current `specHash`
2. Read `ai/readiness/index.json`
3. Find entry for `apiPath`
4. If `specHash` matches: report is **Fresh** — load and display
5. If `specHash` differs: report is **Stale** — load with stale badge + "Re-run AI Insights" CTA
6. If no entry: no report yet — show "Run AI Insights" prompt

#### Retention

Keep latest 10 reports per API path. Prune during new write.

---

## Phase 3: New UI/UX

### 3.1 Analyze view layout

Replace the current single dashboard with a two-section layout.

**Top: Score Header**
- Large score number from static engine only (0–100)
- Band label (e.g., "Ready")
- Sub-label: "Based on static analysis" — no LLM number here
- If AI Insights report exists: small badge "AI Insights available" or "AI Insights stale"

**Middle: Static Engine Panel**
- 4 bucket cards in a grid
- Each card: bucket name, earned/max points, progress bar
- Click to expand: per-rule findings with severity icon, one-line impact, and optional quick-fix button

**Bottom: AI Insights Panel (LLM Engine)**

This panel is clearly labeled "AI Insights — Advisory" and visually separated from the score section to reinforce that it does not affect the number.

*When no report exists:*
- Empty state with "Run AI Insights" button and one-line explanation: "Identify semantic issues an agent would encounter that rules can't detect."

*When report is stale (spec changed since last run):*
- Show previous findings with a "Stale — spec has changed" banner
- Prominent "Re-run AI Insights" CTA

*When report is fresh:*
- **Summary block**: plain English 2–3 sentence summary of biggest agent risks (from the `summary` field)
- **Findings list**: all findings sorted by priority (critical first), grouped by category
  - Each finding shows: operation badge, priority icon, problem statement, confidence tag
  - Expand to show the suggestion + "Apply to spec" button (opens a diff view for user to accept)
- **Category tabs**: Intent Mapping / Ambiguity / Workflow / Actionability — switch to see per-category view
- Timestamp + "Re-run" button

### 3.2 Inline diagnostics

The Spectral runner already produces diagnostics surfaced in the Problems pane. With the new rule IDs, each diagnostic should include:
- Severity (Critical → error, High → warning, Medium/Low → info)
- One-line impact statement appended to the message
- Code action (quick fix) for auto-fixable rules

### 3.3 Commands

Register in `package.json`:
- `AI Readiness: Analyze (Static)` — runs static engine on current file
- `AI Readiness: Run LLM Evaluation` — triggers LLM engine
- `AI Readiness: Export Report` — exports combined JSON or Markdown report
- `AI Readiness: Apply Safe Quick Fixes` — applies all deterministic auto-fixes

### 3.4 Quick fixes (auto-apply safe only)

| Rule | Auto-fix |
|---|---|
| `op-summary-present` | Insert stub summary: `"Summary for <operationId>"` |
| `op-description-present` | Insert stub description |
| `safety-rate-limit-response` | Insert 429 response skeleton with `Retry-After` header |
| `safety-pagination-support` | Insert `page` + `pageSize` query parameter skeletons |
| `gov-tags-description` | Insert `info.description` stub + first tag |

LLM suggestions are never auto-applied. User must click "Apply" per suggestion.

---

## Phase 4: Architecture Changes

### 4.1 New RPC methods

Add to `api-designer-core/src/rpc-types/.../analyze.ts` and register in `governance-manager.ts`:

```ts
// Static readiness (replaces getGovernance for AI readiness path)
getStaticReadiness(req: { filePath: string }): Promise<StaticReadinessResult>

// LLM insights run — returns the report but does NOT affect static score
runAIInsights(req: { filePath: string }): Promise<LLMInsightsReport>

// Load persisted LLM insights report
getAIInsightsReport(req: { filePath: string }): Promise<{ report: LLMInsightsReport | null; status: "fresh" | "stale" | "none" }>

// Export combined report (static findings + LLM insights)
exportReadinessReport(req: { filePath: string; format: "json" | "markdown" }): Promise<{ outputPath: string }>
```

### 4.2 State changes

The current state machine handles file open/close and validation. Add:
- `aiInsightsRunning: boolean` — prevents double triggers
- `aiInsightsStatus: "fresh" | "stale" | "none"` — drives UI freshness display
- `staticReadinessResult: StaticReadinessResult | null`
- `aiInsightsReport: LLMInsightsReport | null`

### 4.3 Spec size handling for LLM

Large specs (>200 operations) should be summarized before sending to LLM:
- Keep all operation metadata (operationId, summary, description)
- Strip request/response body schemas
- Keep error response codes but strip schemas
- Add a note to the LLM prompt that schemas were omitted

Add a setting: `apiDesigner.ai.llm.maxSpecTokens` (default: 32000).

---

## Phase 5: Git and Collaboration Policy

Recommend in the `ai/` folder README:

**Commit these:**
- `ai/readiness/report.json` (latest snapshot — good for team visibility)

**Decide team policy for:**
- `ai/readiness/history/` — can be `.gitignore`d for local-only history

**Never commit:**
- API keys or tokens from examples

---

## Implementation Order

### Sprint 1 (Engine A MVP)
1. Define new types in `analyze.ts`
2. Write new Spectral custom functions for all 21 rules
3. Update `buildAiReadinessSummary()` → `buildStaticReadinessResult()`
4. Update `governance-manager.ts` to use new result type
5. Update `AIReadinessDashboard.tsx` with 4-bucket cards

### Sprint 2 (Engine B MVP)
1. Implement `claude-provider.ts`
2. Write `llm-readiness-runner.ts` with new qualitative prompt
3. Implement `ai/readiness/` folder persistence
4. Add `runAIInsights` and `getAIInsightsReport` RPC methods
5. Add AI Insights panel section to `AIReadinessDashboard.tsx` (separate from score)

### Sprint 3 (Polish)
1. Inline diagnostics with impact statements
2. Quick-fix code actions
3. Export report command
4. Spec truncation for large specs
5. Team settings (strict/balanced profile presets)

---

## Acceptance Criteria

### Static engine
- Runs on every save/edit within 500ms (debounced, same as today)
- Results are deterministic for identical spec input
- All 21 rules produce findings with rule ID, severity, message, and impact statement
- Score is correctly weighted across 4 buckets (max 100)

### LLM engine
- Triggers only on explicit user command
- Returns structured JSON; if JSON is invalid, report `acceptedByChecks: false` and show error state in panel
- LLM output **never** modifies the static score
- Report persisted to `ai/readiness/`; stale detection works correctly on file open

### UI
- Static score stands alone — no LLM number mixed in
- AI Insights panel is visually separate and labeled "Advisory"
- Findings sorted by priority (critical → warn → suggest), grouped by category
- LLM panel shows fresh/stale status and rerun CTA
- Quick fixes apply only to deterministic safe rules
- LLM suggestions require explicit user approval per item (shown as diff, not auto-applied)
