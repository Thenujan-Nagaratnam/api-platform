"""Guard definitions loaded by guardrails-api (see ``run_server.py``).

Before this module imports Hub validators, install them from ``guard/`` with the venv active::

    guardrails hub install hub://guardrails/toxic_language
    guardrails hub install hub://guardrails/detect_pii

Use ``python run_server.py``. Do not run ``pip install guardrails`` (wrong PyPI project).

**Why two guards:** On guardrails-ai 0.10.x, chaining ``DetectPII`` and ``ToxicLanguage`` on a single
``Guard`` is unreliable—depending on order, either toxicity or PII stops being enforced.
Export separate guards and attach **both** policies on the LLM provider (or call ``/validate``
twice with different guard ids).

- ``guard`` — ``my_openai_guard`` — ToxicLanguage (harmful / toxic prompts).
- ``guard_pii`` — ``my_openai_guard_pii`` — DetectPII (email and other Presidio PII).

Paths are ``/guards/{id}/validate``; ``id`` must match WSO2 ``guardName`` for that policy.
"""

from guardrails import Guard
from guardrails.hub import DetectPII
from guardrails.hub import ToxicLanguage

_GUARD_TOXIC_ID = "my_openai_guard"
_GUARD_PII_ID = "my_openai_guard_pii"

guard = Guard(id=_GUARD_TOXIC_ID, name=_GUARD_TOXIC_ID)

guard.use(ToxicLanguage())

guard_pii = Guard(id=_GUARD_PII_ID, name=_GUARD_PII_ID)

guard_pii.use(DetectPII())
