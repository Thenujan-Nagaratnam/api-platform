# API Designer AI Readiness - Unique Final Spec

## Goal

Define a **practical, API Designer-native AI readiness feature** that is unique, easy to explain, and realistic to implement.

This design uses **two separate engines**:

1. **Real-Time Static Readiness** (always on while editing)
2. **On-Demand LLM Evaluation** (runs only when user triggers it)

The product outputs a **simple 100-point score** plus prioritized fixes.

---

## 1. Product Philosophy

This feature is not a clone of any external framework. It is built around day-to-day API authoring needs inside API Designer:

- Fast feedback while writing specs
- Clear fixes instead of abstract grades
- Strict checks for correctness and safety
- Optional deeper reasoning when user explicitly asks for it

---

## 2. Two-Engine Model

## 2.1 Engine A - Real-Time Static Readiness

Runs continuously on file edits. Deterministic and local.

**Purpose**

- Catch structural and agent-critical issues early
- Provide inline diagnostics and quick fixes
- Keep editing loop fast

**Characteristics**

- No remote calls required
- Predictable scoring
- Immediate editor feedback

## 2.2 Engine B - On-Demand LLM Evaluation

Runs only via explicit user action (for example: "Run AI Evaluation").

**Purpose**

- Evaluate semantic clarity and intent quality beyond rigid rules
- Suggest higher-level improvements (naming clarity, workflow guidance, ambiguity reduction)

**Characteristics**

- Triggered manually
- Result is advisory, not blocking
- Stored as a separate report with confidence tags

---

## 3. Scoring Model (Simple 100-Point)

Final score = Static score + LLM bonus/penalty layer.

## 3.1 Static score (0-85 points)

Static analysis owns the core readiness score.

- **Specification Reliability** - 25 points
- **Operation Semantics** - 20 points
- **Agent Execution Safety** - 25 points
- **Governance and Discoverability** - 15 points

## 3.2 LLM evaluation adjustment (-5 to +15 points)

Manual run only.

- Can add up to +15 when semantics are strong and actionable.
- Can subtract up to -5 when there are severe ambiguity risks not captured statically.

This keeps the score grounded in deterministic checks while still benefiting from deeper semantic analysis.

## 3.3 Readiness bands

- `0-39`: Not Ready
- `40-59`: Basic
- `60-74`: Improving
- `75-89`: Ready
- `90-100`: Agent Strong

---

## 4. Real-Time Static Rules (Engine A)

Major Type A has exactly four grouping buckets. Each bucket is designed to cover one practical concern area with minimal overlap.

## 4.1 Specification Reliability (25)

**What this grouping does**  
Ensures the OpenAPI document is structurally trustworthy and machine-parseable before any higher-level evaluation.

**Included checks**

- Spec parse/validation success
- All `$ref` resolvable
- No contradictory schema constraints
- Success and error response schemas present

## 4.2 Operation Semantics (20)

**What this grouping does**  
Improves operation-level clarity so agents can choose the correct endpoint and construct valid calls without guessing.

**Included checks**

- `operationId` present and unique
- `summary` present for all operations
- `description` present for operations and parameters
- Type/format/enum constraints where applicable

## 4.3 Agent Execution Safety (25)

**What this grouping does**  
Validates runtime behavior contracts needed for autonomous retries, pagination, and predictable failure handling.

**Included checks**

- Standardized error body pattern across endpoints
- 429 response + retry guidance (`Retry-After`) where rate limiting applies
- Idempotency declaration for retry-sensitive mutating operations
- Pagination support + correct status code semantics for common CRUD behavior

## 4.4 Governance and Discoverability (15)

**What this grouping does**  
Confirms the API is safely exposable and discoverable for agent usage in team and ecosystem contexts.

**Included checks**

- Security scheme defined and applied
- Sensitive operations are protected and public servers use HTTPS
- No obvious secrets in examples/defaults
- Tags/API-level description/workflow links are present and meaningful

---

## 5. On-Demand LLM Evaluation (Engine B)

Triggered by command: `AI Readiness: Run LLM Evaluation`.

Major Type B also uses exactly four grouping buckets to keep LLM analysis focused and stable.

## 5.1 LLM grouping buckets

### 5.1.1 Intent Mapping

**What this grouping does**  
Checks whether operation text helps an LLM map user goals to the right endpoint reliably.

### 5.1.2 Ambiguity and Collision Detection

**What this grouping does**  
Finds confusingly similar endpoints, vague field names, and likely misrouting hotspots.

### 5.1.3 Workflow Reasoning

**What this grouping does**  
Evaluates whether multi-step flows can be inferred from existing descriptions, links, and response semantics.

### 5.1.4 Documentation Actionability

**What this grouping does**  
Assesses whether descriptions include practical execution guidance (preconditions, side effects, retry expectations).

## 5.2 Output format

- Group scores (0-5 each, one per LLM grouping bucket)
- Top ambiguity hotspots (operation-level)
- Suggested rewritten summaries/descriptions
- Confidence per recommendation (High/Medium/Low)
- Net score adjustment: `-5 .. +15`

## 5.3 Safety and control

- Manual trigger only
- User-visible "this is advisory" label
- Option to disable LLM evaluation globally
- No automatic file edits from LLM output (suggestions only)

---

## 6. UX Design

## 6.1 Editor diagnostics (always-on static)

- Squiggles and Problems pane entries
- Severity levels: Critical, High, Medium, Low
- One-line impact statement ("why this matters for agents")

## 6.2 AI Readiness panel

- Current total score
- Static score vs LLM adjustment shown separately
- Top 5 issues by impact
- Lowest-scoring endpoints

## 6.3 Commands

- `AI Readiness: Analyze (Static)`
- `AI Readiness: Run LLM Evaluation`
- `AI Readiness: Export Report`
- `AI Readiness: Apply Safe Quick Fixes`

---

## 7. Quick Fix Strategy

Auto-fix only deterministic low-risk cases:

- Add missing summary/description stubs
- Insert missing common error response schema reference
- Add missing 429 response skeleton
- Add missing pagination parameter skeleton

LLM suggestions never auto-apply. User approves each suggestion.

---

## 8. Technical Architecture

## 8.1 Components

1. OpenAPI parser/resolver
2. Static rule engine
3. Static scoring engine
4. LLM evaluation runner (manual)
5. Diagnostics + code action adapter
6. Dashboard webview
7. Report exporter

## 8.2 Core interfaces

```ts
type EngineType = "static" | "llm";

interface Finding {
  engine: EngineType;
  id: string;
  severity: "critical" | "high" | "medium" | "low";
  message: string;
  impact: string;
  suggestion?: string;
  location?: string;
}

interface ReadinessScore {
  staticScore: number;      // 0..85
  llmAdjustment: number;    // -5..+15
  totalScore: number;       // 0..100
}
```

---

## 9. Rollout Plan

## Phase 1 (MVP)

- Static engine with top-priority rules
- Inline diagnostics + score panel
- Export static report

## Phase 2

- LLM evaluation command
- Separate LLM report + score adjustment
- Endpoint-level prioritization improvements

## Phase 3

- Better quick-fixes
- Team profile presets (strict vs balanced)
- Optional CI report mode

---

## 10. Acceptance Criteria

## Functional

- Static analysis runs in real-time and updates findings on save/edit.
- LLM evaluation runs only on command.
- UI clearly separates Static score and LLM adjustment.
- Users can export a combined report.

## Quality

- Static run is fast enough for interactive editing.
- Static results are deterministic for same input.
- LLM outputs are tagged with confidence and treated as advisory.

## Safety

- Critical static failures remain visible regardless of LLM results.
- LLM cannot auto-modify OpenAPI without explicit user action.

---

## 11. Why This Is Unique and Reasonable

- Unique because it is built around **two explicit execution modes** (real-time deterministic + manual intelligence), not a monolithic copied framework.
- Reasonable because it prioritizes **what can be shipped reliably first** and keeps costly/non-deterministic evaluation user-triggered.
- Practical because teams get immediate value from static checks and optional deeper insights only when needed.

---

## 12. Addendum - AI Knowledge Store and Report Persistence

This section defines where AI-related artifacts live and how LLM readiness reports are persisted and reloaded.

## 12.1 Dedicated AI knowledge folder

Create a project-level visible folder:

```text
ai/
  INDEX.md
  skill.md
  llms.txt
  llms-full.txt
  apis.json
  readiness/
    index.json
    report.json
    history/
  skills/
  axioms/
```

Purpose of key files:

- `INDEX.md`: lightweight resolver entrypoint with pointers to relevant files.
- `skill.md`: API capability summary for agent consumers.
- `llms.txt`, `llms-full.txt`, and `apis.json`: discovery metadata.
- `readiness/`: LLM evaluation artifacts.
- `skills/`: reusable evaluation workflows/prompts.
- `axioms/`: stable project-specific evaluation principles (optional in early phases).

## 12.2 Report persistence model

Persist reports as files and maintain a fast index.

- **Full reports** stored in `ai/readiness/history/`
- **Latest report snapshot** in `ai/readiness/report.json`
- **Lookup index** in `ai/readiness/index.json`

Each LLM report must include:

- `reportId`
- `apiPath` (workspace-relative)
- `specHash` (SHA-256 of normalized spec content)
- `engineType` (`llm`)
- `generatedAt`
- `model` and `promptVersion` (for LLM runs)
- `scores`, `findings`, `confidence`
- `acceptedByChecks` (boolean for LLM report acceptance criteria)

## 12.3 API identity and freshness

To determine whether a report applies to currently opened API:

- Primary identity: `apiPath`
- Version identity: `specHash`

Freshness logic:

- If `apiPath` matches and `specHash` matches -> report is **Fresh**
- If `apiPath` matches but hash differs -> report is **Stale**

Stale reports remain viewable but must show a visible "Re-run LLM Evaluation" prompt.

## 12.4 Open-file load behavior

When an API file is opened:

1. Compute current `specHash`
2. Read `ai/readiness/index.json`
3. Resolve latest LLM report for `apiPath`
4. Compare report hash to current hash
5. Load fresh report by default; if stale, load with stale badge and rerun CTA

UI must display:

- Last LLM evaluation timestamp
- Fresh/Stale status

## 12.5 Acceptance checks for LLM reports

Because LLM evaluation is latent, enforce deterministic acceptance checks before treating output as trusted:

- Required fields present (scores, findings, confidence, specHash, promptVersion)
- No malformed severity labels
- No empty top-level findings list when penalty was applied
- Report JSON schema valid

If checks fail:

- Mark `acceptedByChecks=false`
- Keep report in history
- Do not apply score adjustment to total score

## 12.6 Retention and cleanup

Default policy:

- Keep latest 10 LLM reports per API
- Prune old reports during new write

Provide settings to override retention limits.

## 12.7 Git and collaboration policy

Recommended defaults:

- Commit `skill.md`, `llms.txt`, `apis.json`, `INDEX.md`
- Decide team policy for readiness artifacts:
  - Either commit summarized markdown reports only
  - Or keep `ai/readiness/` in `.gitignore` for local-only evaluation history

This keeps long-lived AI knowledge sharable while preserving flexibility for report history handling.
