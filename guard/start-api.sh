#!/usr/bin/env sh
# Starts guardrails-api via uvicorn factory (works around `guardrails start` + Typer issue).
set -e
ROOT="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
cd "$ROOT"
exec "${ROOT}/.venv/bin/python" "${ROOT}/run_server.py"
