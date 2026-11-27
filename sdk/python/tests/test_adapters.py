"""M20 agent-framework adapters — OpenAI tool-calling + generic callable map."""
import json

from loopsandbox import LoopSandbox, Response
from loopsandbox.adapters import openai_tools, openai_dispatch, tool_functions


def make_transport(handlers):
    def transport(method, url, headers, body):
        path = "/" + url.split("://", 1)[-1].split("/", 1)[-1]
        for (m, suffix), (status, resp) in handlers.items():
            if method == m and path.endswith(suffix):
                return Response(status, json.dumps(resp).encode())
        return Response(404, b'{"code":"not_found","message":"no route"}')
    return transport


def _sandbox():
    t = make_transport({
        ("POST", "/v1/sandboxes"): (201, {"id": "sbx_1"}),
        ("POST", "/sessions"): (201, {"session_id": "ses_1"}),
        ("POST", "/execute"): (200, {"exit_code": 0, "stdout": "hello-agent\n", "stderr": ""}),
    })
    return LoopSandbox("lsb_k", "https://api.test", transport=t).create_sandbox()


def test_openai_tools_schema_shape():
    tools = openai_tools()
    names = {t["function"]["name"] for t in tools}
    assert names == {"sandbox_run_code", "sandbox_run", "sandbox_write_file", "sandbox_read_file"}
    for t in tools:
        assert t["type"] == "function"
        assert "parameters" in t["function"]  # valid JSON-schema params


def test_openai_dispatch_runs_code_against_sandbox():
    sbx = _sandbox()
    out = openai_dispatch(sbx, "sandbox_run_code", {"code": "print('hi')"})
    assert out.strip() == "hello-agent"


def test_openai_dispatch_unknown_tool_is_error():
    sbx = _sandbox()
    try:
        openai_dispatch(sbx, "sandbox_delete_everything", {})
        assert False, "expected ValueError"
    except ValueError:
        pass


def test_tool_functions_map():
    sbx = _sandbox()
    fns = tool_functions(sbx)
    assert set(fns) == {"sandbox_run_code", "sandbox_run", "sandbox_write_file", "sandbox_read_file"}
    assert fns["sandbox_run_code"]("print('x')").strip() == "hello-agent"
