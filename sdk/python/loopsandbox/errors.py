"""Typed errors raised by the LoopSandbox SDK.

Every failed HTTP answer becomes a LoopSandboxError subclass chosen by status code. The
API error body is {"error": {"code": ..., "message": ...}}.
"""
from __future__ import annotations

import json
from typing import Mapping, Optional

__all__ = [
    "LoopSandboxError",
    "ValidationError",
    "UnauthorizedError",
    "QuotaExceededError",
    "ForbiddenError",
    "NotFoundError",
    "ConflictError",
    "PayloadTooLargeError",
    "RateLimitError",
    "UnsupportedError",
    "OperationTimeoutError",
    "ServerError",
    "StreamError",
    "error_from_response",
]

_MAX_RAW_MESSAGE_CHARS = 500


class LoopSandboxError(Exception):
    """Base error. status is the HTTP status, code the API error code."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(f"{status} {code}: {message}")
        self.status, self.code, self.message = status, code, message


class ValidationError(LoopSandboxError):
    """400 or 422: the request was refused as invalid."""


class UnauthorizedError(LoopSandboxError):
    """401: the API key is missing, unknown or revoked."""


class QuotaExceededError(LoopSandboxError):
    """402: a plan quota was reached."""


class ForbiddenError(LoopSandboxError):
    """403: the credential lacks the scope of the route, or a signed URL is invalid."""


class NotFoundError(LoopSandboxError):
    """404: the resource does not exist for this tenant."""


class ConflictError(LoopSandboxError):
    """409: the resource is in a state that refuses the operation. See code."""


class PayloadTooLargeError(LoopSandboxError):
    """413: the request body is above the limit of the route."""


class RateLimitError(LoopSandboxError):
    """429: the API key went beyond its request rate.

    retry_after_seconds is the value of the Retry-After header, or None when the
    answer carried no usable one.
    """

    def __init__(self, status: int, code: str, message: str, retry_after_seconds: Optional[float] = None):
        super().__init__(status, code, message)
        self.retry_after_seconds = retry_after_seconds


class UnsupportedError(LoopSandboxError):
    """501: the deployment does not support this capability. See get_capabilities()."""


class OperationTimeoutError(LoopSandboxError):
    """504: the command ran past its timeout."""


class ServerError(LoopSandboxError):
    """Any other 5xx answer."""


class StreamError(LoopSandboxError):
    """A streamed execution failed after the 200 status was already sent."""


_BY_STATUS = {
    400: ValidationError,
    401: UnauthorizedError,
    402: QuotaExceededError,
    403: ForbiddenError,
    404: NotFoundError,
    409: ConflictError,
    413: PayloadTooLargeError,
    422: ValidationError,
    429: RateLimitError,
    501: UnsupportedError,
    504: OperationTimeoutError,
}


def _header(headers: Mapping[str, str], name: str) -> Optional[str]:
    wanted = name.lower()
    for key, value in headers.items():
        if key.lower() == wanted:
            return value
    return None


def _retry_after_seconds(headers: Mapping[str, str]) -> Optional[float]:
    raw = _header(headers, "Retry-After")
    if raw is None:
        return None
    try:
        seconds = float(raw.strip())
    except ValueError:
        # The API sends whole seconds. An HTTP date from a proxy is not converted.
        return None
    return seconds if seconds >= 0 else None


def _code_and_message(body: bytes) -> tuple[str, str]:
    try:
        payload = json.loads(body) if body else None
    except ValueError:
        # Not JSON, as from a proxy in front of the API: keep the text as the message.
        return "error", body.decode(errors="replace")[:_MAX_RAW_MESSAGE_CHARS]
    if not isinstance(payload, dict):
        return "error", ""
    nested = payload.get("error")
    detail = nested if isinstance(nested, dict) else payload
    return str(detail.get("code") or "error"), str(detail.get("message") or "")


def error_from_response(status: int, body: bytes, headers: Optional[Mapping[str, str]] = None) -> LoopSandboxError:
    """Build the typed error for a failed HTTP answer."""
    code, message = _code_and_message(body)
    cls = _BY_STATUS.get(status)
    if cls is None:
        cls = ServerError if status >= 500 else LoopSandboxError
    if cls is RateLimitError:
        return RateLimitError(status, code, message, _retry_after_seconds(headers or {}))
    return cls(status, code, message)
