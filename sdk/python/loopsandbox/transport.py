"""The injectable HTTP boundary of the SDK, built on the standard library."""
from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Callable, Iterable, Optional

__all__ = ["Response", "StreamResponse", "Transport", "StreamTransport"]


@dataclass
class Response:
    status: int
    body: bytes
    headers: dict = field(default_factory=dict)

    def json(self) -> dict:
        return json.loads(self.body) if self.body else {}


@dataclass
class StreamResponse:
    """An answer whose body is read line by line while it arrives."""

    status: int
    lines: Iterable[bytes]
    headers: dict = field(default_factory=dict)
    close: Optional[Callable[[], None]] = None


# Transport is the injectable HTTP boundary: (method, url, headers, body) -> Response.
Transport = Callable[[str, str, dict, Optional[bytes]], Response]

# StreamTransport is the same boundary for Server-Sent Events answers.
StreamTransport = Callable[[str, str, dict, Optional[bytes]], StreamResponse]


def default_transport(method: str, url: str, headers: dict, body: Optional[bytes]) -> Response:
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req) as r:  # noqa: S310 - the caller's own API base URL
            return Response(r.status, r.read(), dict(r.headers.items()))
    except urllib.error.HTTPError as e:
        return Response(e.code, e.read(), dict(e.headers.items()))


def default_stream_transport(method: str, url: str, headers: dict, body: Optional[bytes]) -> StreamResponse:
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        r = urllib.request.urlopen(req)  # noqa: S310 - the caller's own API base URL
    except urllib.error.HTTPError as e:
        return StreamResponse(e.code, [e.read()], dict(e.headers.items()))
    return StreamResponse(r.status, r, dict(r.headers.items()), r.close)
