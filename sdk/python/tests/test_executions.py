"""Commands, code sessions and their streamed variants."""
import json

import pytest

from loopsandbox import (
    CellResult,
    ExecResult,
    LoopSandbox,
    Sandbox,
    Session,
    StreamError,
    StreamEvent,
    StreamResponse,
    UnsupportedError,
)


def sse(*events):
    """Encode (event, data) pairs as the lines of a Server-Sent Events body."""
    lines = []
    for name, data in events:
        lines += [f"event: {name}\n".encode(), f"data: {json.dumps(data)}\n".encode(), b"\n"]
    return lines


class FakeStream:
    """A stream transport double that records the request and replays one answer."""

    def __init__(self, lines, status=200):
        self.lines, self.status = lines, status
        self.calls = []
        self.closed = False

    def __call__(self, method, url, headers, body):
        self.calls.append((method, url, headers, body))
        return StreamResponse(self.status, self.lines, {}, self._close)

    def _close(self):
        self.closed = True


def streaming_sandbox(stream):
    client = LoopSandbox("lsb_k", "https://api.test", stream_transport=stream)
    return Sandbox(client, "sbx_1")


def test_run_sends_command_workdir_env_and_timeout(api):
    api.answer(200, {"exit_code": 0, "stdout": "", "stderr": ""})

    api.sandbox().run(["ls", "-l"], "/workspace/src", env={"A": "1"}, timeout_ms=2500)

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/executions")
    assert api.last.json == {"cmd": ["ls", "-l"], "workdir": "/workspace/src", "env": {"A": "1"}, "timeout_ms": 2500}


def test_run_returns_non_zero_exit_as_a_normal_result(api):
    api.answer(200, {"exit_code": 2, "stdout": "", "stderr": "no such file\n"})

    result = api.sandbox().run(["cat", "missing"])

    assert result == ExecResult(2, "", "no such file\n")


def test_run_stream_posts_with_stream_query_and_bearer_key():
    stream = FakeStream(sse(("done", {"exit_code": 0})))

    list(streaming_sandbox(stream).run_stream(["echo", "hi"], timeout_ms=1000))

    method, url, headers, body = stream.calls[0]
    assert (method, url) == ("POST", "https://api.test/v1/sandboxes/sbx_1/executions?stream=1")
    assert headers["Authorization"] == "Bearer lsb_k"
    assert json.loads(body) == {"cmd": ["echo", "hi"], "timeout_ms": 1000}


def test_run_stream_yields_output_chunks_then_done_with_exit_code():
    stream = FakeStream(sse(
        ("stdout", {"data": "one\n"}), ("stderr", {"data": "warn\n"}), ("done", {"exit_code": 3}),
    ))

    events = list(streaming_sandbox(stream).run_stream(["sh", "-c", "x"]))

    assert events == [
        StreamEvent("stdout", data="one\n"),
        StreamEvent("stderr", data="warn\n"),
        StreamEvent("done", exit_code=3),
    ]


def test_run_stream_raises_stream_error_on_error_event():
    stream = FakeStream(sse(("stdout", {"data": "partial"}), ("error", {"error": "runtime died", "code": "error"})))

    with pytest.raises(StreamError) as raised:
        list(streaming_sandbox(stream).run_stream(["sleep", "9"]))

    assert raised.value.message == "runtime died"


def test_run_stream_raises_stream_error_when_stream_ends_without_done():
    stream = FakeStream(sse(("stdout", {"data": "partial"})))

    with pytest.raises(StreamError) as raised:
        list(streaming_sandbox(stream).run_stream(["sleep", "9"]))

    assert raised.value.code == "stream_interrupted"


def test_run_stream_raises_typed_error_when_request_is_refused():
    body = json.dumps({"error": {"code": "not_implemented", "message": "streamed exec is not supported"}}).encode()
    stream = FakeStream([body], status=501)

    with pytest.raises(UnsupportedError) as raised:
        list(streaming_sandbox(stream).run_stream(["echo", "hi"]))

    assert raised.value.code == "not_implemented"


def test_run_stream_closes_the_connection_after_done():
    stream = FakeStream(sse(("done", {"exit_code": 0})))

    list(streaming_sandbox(stream).run_stream(["true"]))

    assert stream.closed is True


def test_start_session_returns_session_handle(api):
    api.answer(201, {"session_id": "ses_1"})

    session = api.sandbox().start_session()

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/sessions")
    assert session.id == "ses_1"


def test_session_execute_posts_code_and_decodes_cell_result(api):
    api.answer(200, {"exit_code": 0, "stdout": "42\n", "stderr": ""})

    result = Session(api.client, "sbx_1", "ses_1").execute("print(6 * 7)")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/sessions/ses_1/execute")
    assert api.last.json == {"code": "print(6 * 7)"}
    assert result == CellResult(0, "42\n", "")


def test_session_close_deletes_the_session(api):
    api.answer(204)

    Session(api.client, "sbx_1", "ses_1").close()

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/sandboxes/sbx_1/sessions/ses_1")


def test_session_execute_stream_posts_code_with_stream_query():
    stream = FakeStream(sse(("done", {"exit_code": 0, "stderr": ""})))
    client = LoopSandbox("lsb_k", "https://api.test", stream_transport=stream)

    list(Session(client, "sbx_1", "ses_1").execute_stream("x = 1"))

    method, url, _, body = stream.calls[0]
    assert (method, url) == ("POST", "https://api.test/v1/sandboxes/sbx_1/sessions/ses_1/execute?stream=1")
    assert json.loads(body) == {"code": "x = 1"}


def test_session_execute_stream_done_event_carries_exit_code_and_stderr():
    stream = FakeStream(sse(("stdout", {"data": "a"}), ("done", {"exit_code": 1, "stderr": "ValueError: boom"})))
    client = LoopSandbox("lsb_k", "https://api.test", stream_transport=stream)

    events = list(Session(client, "sbx_1", "ses_1").execute_stream("raise ValueError('boom')"))

    assert events[-1] == StreamEvent("done", exit_code=1, stderr="ValueError: boom")
