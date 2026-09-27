# Specification Quality Checklist: A Per-Attempt Retry Hop Any Policy Can Use

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-27
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Notes

- The users here are policy authors and operators, so "policy definition", "policy SDK" and "gateway" are the product's own vocabulary, not implementation detail. Envoy, xDS and listener names are kept out of the requirements.
- No clarification needed: the defaults with more than one reading have reasonable choices recorded in Assumptions and Edge Cases:
  - retry-on-unauthorized is off by default;
  - after a second 401 the client gets a fixed gateway error;
  - the gateway-wide cap is configurable.
- SC-001 and SC-003 are verifiable from change sets and code search.
