"""Unit tests with a mock transport — mirror the TS SDK's mocked-fetch tests."""
import json

from loopsandbox import LoopSandbox, NotFoundError, Response


def make_transport(handlers):
    """handlers: dict of (method, path_suffix) -> (status, body_dict)."""
    calls = []

    def transport(method, url, headers, body):
        path = url.split("://", 1)[-1].split("/", 1)[-1]
        path = "/" + path
        calls.append(f"{method} {path}")
        for (m, suffix), (status, resp) in handlers.items():
            if method == m and path.endswith(suffix):
                return Response(status, json.dumps(resp).encode())
        return Response(404, b'{"code":"not_found","message":"no route"}')

    transport.calls = calls
    return transport


def test_run_code_reuses_one_default_session():
    t = make_transport({
        ("POST", "/v1/sandboxes"): (201, {"id": "sbx_1"}),
        ("POST", "/sessions"): (201, {"session_id": "ses_1"}),
        ("POST", "/execute"): (200, {"exit_code": 0, "stdout": "42\n", "stderr": ""}),
    })
    c = LoopSandbox("lsb_k", "https://api.test", transport=t)
    sbx = c.create_sandbox()
    r1 = sbx.run_code("x = 41")
    r2 = sbx.run_code("print(x + 1)")
    assert r1.exit_code == 0
    assert r2.stdout.strip() == "42"
    # exactly one session opened (default session reused), two executes
    assert t.calls.count("POST /v1/sandboxes/sbx_1/sessions") == 1
    assert t.calls.count("POST /v1/sandboxes/sbx_1/sessions/ses_1/execute") == 2


def test_cell_exception_is_normal_result():
    t = make_transport({
        ("POST", "/v1/sandboxes"): (201, {"id": "sbx_1"}),
        ("POST", "/sessions"): (201, {"session_id": "ses_1"}),
        ("POST", "/execute"): (200, {"exit_code": 1, "stdout": "", "stderr": "ValueError: boom"}),
    })
    c = LoopSandbox("lsb_k", "https://api.test", transport=t)
    sbx = c.create_sandbox()
    r = sbx.run_code("raise ValueError('boom')")
    assert r.exit_code == 1
    assert "ValueError: boom" in r.stderr


def test_fork_returns_new_sandbox():
    t = make_transport({
        ("POST", "/v1/sandboxes"): (201, {"id": "sbx_1"}),
        ("POST", "/fork"): (201, {"id": "sbx_2"}),
    })
    c = LoopSandbox("lsb_k", "https://api.test", transport=t)
    sbx = c.create_sandbox()
    fork = sbx.fork()
    assert fork.id == "sbx_2" and fork.id != sbx.id


def test_not_found_raises_typed_error():
    def transport(method, url, headers, body):
        return Response(404, b'{"code":"not_found","message":"sandbox not found"}')

    c = LoopSandbox("lsb_k", "https://api.test", transport=transport)
    try:
        c.create_sandbox()
        assert False, "expected NotFoundError"
    except NotFoundError as e:
        assert e.status == 404
