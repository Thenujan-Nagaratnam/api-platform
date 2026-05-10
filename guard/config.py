from guardrails import Guard
from guardrails.hub import ToxicLanguage

_GUARD_TOXIC_ID = "toxic-language-guard"

guard = Guard(id=_GUARD_TOXIC_ID, name=_GUARD_TOXIC_ID)

guard.use(ToxicLanguage())
