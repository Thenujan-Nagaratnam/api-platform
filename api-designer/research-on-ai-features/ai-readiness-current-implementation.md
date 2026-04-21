# API Designer AI Readiness Specification

## 1. Purpose

Define the current AI readiness capabilities implemented in API Designer, including validation logic, scoring behavior, data contracts, and UI behavior. This spec documents the feature set as it exists today so product, engineering, and QA teams can align on expected behavior.

## 2. Scope

### In scope

- AI readiness analysis for API specifications in the Analyze view.
- Spectral-based validation using the dedicated AI readiness ruleset.
- Category-level coverage metrics and missing-item insights.
- AI-assisted remediation triggers from the dashboard.

### Out of scope

- Authoring new AI readiness rules.
- Rule weighting customization by users.
- Policy/version management for multiple AI readiness profiles.
- Non-Spectral AI quality scoring models.

## 3. High-Level Feature Summary

API Designer provides a dedicated AI readiness flow that:

1. Selects an AI readiness ruleset for the current API.
2. Runs Spectral validation against the current API file.
3. Collects category coverage metrics through custom Spectral functions.
4. Produces a normalized summary model for UI consumption.
5. Renders an AI Readiness dashboard with:
   - Overall score
   - Four category metrics
   - Missing-item details
   - "Fix with AI" and "Fix All with AI" actions

## 4. Functional Architecture

### 4.1 Components

- `api-designer-core`
  - Provides default ruleset descriptors.
  - Defines RPC types for governance and AI readiness summary payloads.
  - Builds AI readiness summary and fallback score behavior.
- `api-designer-extension`
  - Resolves/loads ruleset content.
  - Runs Spectral validation.
  - Injects custom AI readiness functions and collects metrics.
  - Exposes governance and applicable-ruleset RPC methods.
- `api-designer-visualizer`
  - Calls RPC methods to fetch rulesets and governance results.
  - Renders AI readiness score, category breakdown, and missing details.
  - Emits AI-fix prompts to Copilot chat.

### 4.2 End-to-End Flow

1. UI requests `getApplicableRulesets(filePath)`.
2. Backend returns governance rulesets plus one `aiReadinessRuleset`.
3. UI calls `getGovernance(filePath, name, ruleset)` using that ruleset.
4. Backend loads and processes the ruleset, then runs Spectral.
5. If ruleset name contains "ai readiness", backend:
   - Instantiates `AiReadinessMetricsCollector`
   - Replaces AI function names with executable functions
6. Backend returns governance result with:
   - Violations
   - Rule score
   - `aiReadinessMetrics`
   - `aiReadinessSummary`
7. UI displays score and category tiles; missing items open in modal.

## 5. Ruleset Specification

### 5.1 Default AI readiness ruleset

- Name: `WSO2 REST API AI Readiness Guidelines`
- Default file: `ai-readiness.yaml`
- Default source folder: GitHub ruleset catalog (configured in core constants)
- Ruleset content path: `rulesetContent`

### 5.2 Rule families currently enforced

The current `ai-readiness.yaml` contains checks for:

- API-level description and contact metadata
- Operation-level summary, description, operationId, and tags
- Parameter descriptions and examples
- Request body descriptions and examples
- Response descriptions and examples
- 4xx and 5xx error response conventions
- Error response schema availability
- Schema and schema-property descriptions/examples
- Enum field descriptive guidance
- Success response presence (2xx)
- Security scheme definition and description
- Tag descriptions and external docs
- Response content type
- Polymorphism discriminator guidance
- Deprecation messaging guidance
- Server description guidance

### 5.3 Metrics-tracked categories

Only checks using custom functions are tracked in category coverage:

- `summaries`
- `descriptions`
- `examples`
- `errorResponses`

Checks implemented with standard Spectral functions still appear as violations but do not contribute to these category totals unless explicitly mapped through custom AI readiness functions.

## 6. Data Contracts

### 6.1 AI readiness metrics

`AiReadinessMetrics` contains:

- `categories: Record<string, AiReadinessCoverage>`

Each category coverage contains:

- `total: number`
- `passed: number`
- `failed: number`
- `passedPaths: string[][]`
- `failedPaths: string[][]`

### 6.2 AI readiness summary

`AiReadinessSummary` contains:

- `score: number` (0-100)
- `summariesComplete: AiReadinessCategorySummary`
- `descriptionsComplete: AiReadinessCategorySummary`
- `schemasWithExamples: AiReadinessCategorySummary`
- `errorResponsesDefined: AiReadinessCategorySummary`
- optional `validation.violations: AiReadinessViolation[]`

Each category summary contains:

- `filled: number`
- `total: number`
- `percentage: number` (0-100)
- optional `missing: AiReadinessViolation[]`

Each violation contains:

- `pathSegments: string[]`
- `displayPath: string`
- `message: string`

## 7. Scoring and Coverage Rules

### 7.1 Category coverage calculation

For each metrics category:

- `total` is the number of evaluated nodes for that category.
- `passed` is the number of nodes that satisfy the check.
- `failed` is the number of nodes that do not satisfy the check.
- `percentage = round((passed / total) * 100)` when `total > 0`.

### 7.2 Readiness score precedence

The final score used by AI readiness consumers follows this precedence:

1. `aiReadinessSummary.score` (if available)
2. `computeReadinessScoreFromMetrics(aiReadinessMetrics)` (if possible)
3. governance-level score from failed/passed rules

### 7.3 Fallback behavior

If category metrics are missing, summary computation falls back to violation-derived totals and estimated category partitioning.

## 8. UI/UX Behavior

### 8.1 Analyze dashboard

- Header: `AI Readiness Analysis`
- Score pill and hero score section
- Four metric tiles:
  - Summaries
  - Descriptions
  - Examples
  - Error Responses

### 8.2 Tile interaction

- Tile is interactive only when missing items exist.
- Clicking opens a modal focused on that category tab.

### 8.3 Missing-items modal

- Tabs for all four categories with counts.
- List of missing items showing message and path.
- Empty state if no missing items in selected tab.

### 8.4 AI fix actions

- Per item: `Fix with AI`
- Per category: `Fix All with AI`
- Actions send context and prompt to Copilot chat, including ruleset and file information required to rerun validation iteratively.

## 9. Error Handling and Resilience

- If RPC client is unavailable, dashboard renders default zero-state data with error messaging.
- If file path is invalid, dashboard returns safe empty/default result.
- If ruleset is unavailable, dashboard still renders with zeroed metrics.
- Validation pipeline logs and throws processing errors; UI displays failure message while preserving renderability.

## 10. Non-Functional Characteristics

- Validation engine: Spectral lint run over parsed YAML document.
- Ruleset source supports remote (GitHub/raw URL) and local file path resolution.
- Path-level metric capture enables targeted remediation prompts.

## 11. Limitations (Current)

- AI readiness activation is inferred from ruleset name containing `"ai readiness"`.
- Category model is fixed at four buckets even though ruleset has broader checks.
- Default behavior depends on externally hosted ruleset content.
- Severity levels (`error`, `warn`, `info`, `hint`) are surfaced, but category completion is based on custom function pass/fail outcomes.

## 12. Acceptance Criteria (Current Implementation)

1. Given a valid OpenAPI spec and available AI ruleset, Analyze view shows an AI readiness score and four category tiles.
2. Missing summaries/descriptions/examples/error responses appear under the correct category tab.
3. For each category, displayed `filled/total/percentage` reflects collected metrics when available.
4. Clicking `Fix with AI` includes item context and path in chat prompt context.
5. Clicking `Fix All with AI` includes ruleset metadata and instructs iterative validation/fix flow.
6. If validation fails, dashboard remains visible with error banner and safe default data.
7. If no missing items in a category, modal displays completion state.

## 13. Future Improvement Candidates (Informational)

- Introduce explicit ruleset metadata flag for AI readiness instead of name-based detection.
- Add configurable category weighting and score formula transparency.
- Expose per-rule contribution to category completion in UI.
- Support version pinning and provenance display for remote rulesets.

