"""Run guardrails-api without ``guardrails start``.

``guardrails start`` delegates to ``guardrails_api.cli.start.start()`` in a way Typer
binds incorrectly against guardrails-api 0.4.x (``middleware`` becomes ``OptionInfo``).
This module calls ``guardrails_api.app.create_app`` directly and patches that bug.

Validate requests must populate ``llm_output`` (snake_case) or guardrails-api falls through
to the chat path and raises "messages is empty". This stack wraps the app with pure ASGI
middleware (not ``BaseHTTPMiddleware``, which can drop rewritten bodies) that:

- Maps ``llmOutput`` / ``numReasks`` (camelCase) to snake_case.
- If still missing, sets ``llm_output`` from the last OpenAI-style ``messages[].content``.
- Drops ``messages`` and ``model`` once ``llm_output`` is set: guardrails-api forwards leftover
  OpenAI fields into ``AsyncGuard.parse`` and keeping ``messages`` plus ``llm_output`` skips Hub
  validators such as ToxicLanguage (text appears to "pass" when it should fail).

Usage::

    cd guard && .venv/bin/python run_server.py

Environment:

- ``PORT`` — listen port (default 8000)
- ``UVICORN_HOST`` — bind address (default 127.0.0.1)
- ``GR_CONFIG_FILE_PATH`` — defaults to ``config.py`` beside this file
- ``GR_ENV_FILE`` — optional ``.env`` in ``guard/`` if present
"""

from __future__ import annotations

import errno
import json
import os
import socket
import sys
from pathlib import Path

import uvicorn
from starlette.datastructures import MutableHeaders


def _exit_if_port_in_use(host: str, port: int) -> None:
    """Fail before loading ``config.py`` (heavy Hub/models) if nothing can listen here."""
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            s.bind((host, port))
    except OSError as e:
        if e.errno in (errno.EADDRINUSE, errno.EACCES):
            print(
                f"Cannot bind {host}:{port} ({e.strerror}). "
                f"Use another port, e.g. PORT=8001 {sys.argv[0]}, "
                f"or stop the process using this port (macOS: `lsof -nP -iTCP:{port} -sTCP:LISTEN`).",
                file=sys.stderr,
            )
            raise SystemExit(1) from e
        raise


def _patch_guardrails_optioninfo_middleware() -> None:
    """guardrails-api passes Typer's OptionInfo into ``register_middleware`` when invoked wrong."""
    import guardrails_api.app as app_module

    _orig = app_module.register_middleware

    def _safe_register_middleware(*, middleware=None, app=None):
        try:
            from typer.models import OptionInfo
        except ImportError:
            OptionInfo = None  # type: ignore[misc,assignment]

        if OptionInfo is not None and isinstance(middleware, OptionInfo):
            middleware = None
        return _orig(middleware=middleware, app=app)

    app_module.register_middleware = _safe_register_middleware  # type: ignore[assignment]


def _normalize_validate_json_body(raw: bytes) -> bytes:
    """Return JSON bytes suitable for ValidateRequest (``llm_output`` populated when possible)."""
    if not raw:
        return raw

    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return raw

    if not isinstance(data, dict):
        return raw

    if "llm_output" not in data and "llmOutput" in data:
        data["llm_output"] = data.pop("llmOutput")
    if "num_reasks" not in data and "numReasks" in data:
        data["num_reasks"] = data.pop("numReasks")

    if data.get("llm_output") is None:
        msgs = data.get("messages")
        if isinstance(msgs, list) and msgs:
            last = msgs[-1]
            if isinstance(last, dict):
                content = last.get("content")
                if isinstance(content, str) and content.strip():
                    data["llm_output"] = content

    # Prevent guardrails-api from passing chat-completion kwargs into ``parse`` alongside text.
    if data.get("llm_output") is not None:
        data.pop("messages", None)
        data.pop("model", None)

    try:
        return json.dumps(data, separators=(",", ":")).encode("utf-8")
    except (TypeError, ValueError):
        return raw


class NormalizeValidateBodyASGI:
    """Pure ASGI middleware — reliably replaces POST body for ``/guards/*/validate``."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):  # noqa: ANN001, ANN204
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        method = scope.get("method", "")
        path = scope.get("path", "")
        if (
            method != "POST"
            or "/guards/" not in path
            or not path.rstrip("/").endswith("/validate")
        ):
            await self.app(scope, receive, send)
            return

        body = b""
        more_body = True
        while more_body:
            message = await receive()
            if message["type"] != "http.request":
                await self.app(scope, receive, send)
                return
            body += message.get("body", b"")
            more_body = message.get("more_body", False)

        new_body = _normalize_validate_json_body(body)

        new_headers = [
            (name, value)
            for name, value in scope.get("headers") or []
            if name.lower() != b"content-length"
        ]
        new_scope = {**scope, "headers": new_headers}
        MutableHeaders(scope=new_scope)["content-length"] = str(len(new_body))

        async def new_receive():  # noqa: ANN202
            return {"type": "http.request", "body": new_body, "more_body": False}

        await self.app(new_scope, new_receive, send)


def create_app():  # noqa: ANN201
    """Uvicorn factory: guardrails app + ASGI wrapper / patches."""
    _patch_guardrails_optioninfo_middleware()
    from guardrails_api.app import create_app as guardrails_create_app

    return NormalizeValidateBodyASGI(guardrails_create_app())


def main() -> None:
    root = Path(__file__).resolve().parent
    os.chdir(root)
    os.environ.setdefault("GR_CONFIG_FILE_PATH", str(root / "config.py"))
    env_path = root / ".env"
    if env_path.is_file():
        os.environ.setdefault("GR_ENV_FILE", str(env_path))

    host = os.environ.get("UVICORN_HOST", "127.0.0.1")
    port = int(os.environ.get("PORT", "8000"))
    _exit_if_port_in_use(host, port)
    uvicorn.run(
        "run_server:create_app",
        factory=True,
        host=host,
        port=port,
    )


if __name__ == "__main__":
    main()
