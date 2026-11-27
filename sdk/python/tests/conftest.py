"""Shared test doubles: a transport that records each request and replays queued answers."""
import json
from dataclasses import dataclass
from typing import Optional

import pytest

from loopsandbox import LoopSandbox, Response, Sandbox

BASE_URL = "https://api.test"


@dataclass
class Call:
    method: str
    path: str  # path and query string, without the base URL
    headers: dict
    body: Optional[bytes]

    @property
    def json(self):
        return json.loads(self.body)


class FakeApi:
    """A transport double. Answers are replayed in the order they were queued."""

    def __init__(self):
        self.calls = []
        self._answers = []
        self.client = LoopSandbox("lsb_k", BASE_URL, transport=self._transport)

    def answer(self, status, body=None, headers=None):
        """Queue one answer. A dict or list body is sent as JSON, bytes as they are."""
        if body is None:
            raw = b""
        elif isinstance(body, bytes):
            raw = body
        else:
            raw = json.dumps(body).encode()
        self._answers.append(Response(status, raw, headers or {}))

    def error(self, status, code, message="", headers=None):
        self.answer(status, {"error": {"code": code, "message": message}}, headers)

    def sandbox(self, sandbox_id="sbx_1"):
        return Sandbox(self.client, sandbox_id)

    @property
    def last(self):
        return self.calls[-1]

    def _transport(self, method, url, headers, body):
        assert self._answers, f"unexpected request: {method} {url}"
        self.calls.append(Call(method, url[len(BASE_URL):] if url.startswith(BASE_URL) else url, headers, body))
        return self._answers.pop(0)


@pytest.fixture
def api():
    return FakeApi()
