from guardrails import Guard
from guardrails.hub import RegexMatch
from guardrails.hub import ToxicLanguage

guard = Guard()
guard.name = "my_openai_guard"

# Allows normal chat text (letters, numbers, spaces, punctuation)
guard.use(RegexMatch(regex=r"^[\s\S]{1,4000}$"))
guard.use(ToxicLanguage())