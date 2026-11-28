"""SandboxHandle lifecycle: create, list, get, timeout, fork, pause, resume, destroy, metrics, logs."""
from loopsandbox import SandboxInfo, SandboxLogs, SandboxMetrics

SANDBOX = {
    "id": "sbx_1",
    "tenant_id": "tenant-a",
    "image": "python",
    "state": "running",
    "created_at": "2026-11-20T10:00:00Z",
    "expires_at": "2026-11-20T10:05:00Z",
    "metadata": {"team": "search"},
    "env": {"MODE": "ci"},
}


def test_create_sandbox_sends_every_option_in_the_body(api):
    api.answer(201, SANDBOX)

    api.client.create_sandbox(
        image="python", ttl_seconds=120, template="tpl_1", metadata={"team": "search"}, env={"MODE": "ci"}
    )

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes")
    assert api.last.json == {
        "image": "python",
        "ttl_seconds": 120,
        "template": "tpl_1",
        "metadata": {"team": "search"},
        "env": {"MODE": "ci"},
    }


def test_create_sandbox_without_options_sends_an_empty_object(api):
    api.answer(201, SANDBOX)

    api.client.create_sandbox()

    assert api.last.json == {}


def test_create_sandbox_sends_gpu_class(api):
    api.answer(201, SANDBOX)

    api.client.create_sandbox(gpu="a10")

    assert api.last.json == {"gpu": "a10"}


def test_create_sandbox_returns_handle_with_decoded_info(api):
    api.answer(201, SANDBOX)

    sandbox = api.client.create_sandbox()

    assert sandbox.id == "sbx_1"
    assert sandbox.info == SandboxInfo(
        id="sbx_1",
        tenant_id="tenant-a",
        state="running",
        image="python",
        created_at="2026-11-20T10:00:00Z",
        expires_at="2026-11-20T10:05:00Z",
        metadata={"team": "search"},
        env={"MODE": "ci"},
    )


def test_create_sandbox_sends_idempotency_key_header(api):
    api.answer(201, SANDBOX)

    api.client.create_sandbox(idempotency_key="order-42")

    assert api.last.headers["Idempotency-Key"] == "order-42"


def test_create_sandbox_without_idempotency_key_sends_no_such_header(api):
    api.answer(201, SANDBOX)

    api.client.create_sandbox()

    assert "Idempotency-Key" not in api.last.headers


def test_requests_authenticate_with_bearer_api_key(api):
    api.answer(200, SANDBOX)

    api.client.get_sandbox("sbx_1")

    assert api.last.headers["Authorization"] == "Bearer lsb_k"


def test_list_sandboxes_without_filters_sends_no_query(api):
    api.answer(200, {"sandboxes": []})

    api.client.list_sandboxes()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes")


def test_list_sandboxes_encodes_filters_limit_and_cursor(api):
    api.answer(200, {"sandboxes": []})

    api.client.list_sandboxes(
        states=["running", "paused"], metadata={"team": "search", "env": "ci"}, limit=10, cursor="abc"
    )

    assert api.last.path == (
        "/v1/sandboxes?state=running%2Cpaused&metadata=team%3Dsearch&metadata=env%3Dci&limit=10&cursor=abc"
    )


def test_list_sandboxes_returns_handles_and_next_cursor(api):
    api.answer(200, {"sandboxes": [SANDBOX, {**SANDBOX, "id": "sbx_2"}], "next_cursor": "c2"})

    page = api.client.list_sandboxes()

    assert [s.id for s in page.sandboxes] == ["sbx_1", "sbx_2"]
    assert page.sandboxes[0].info.state == "running"
    assert page.next_cursor == "c2"


def test_list_sandboxes_last_page_has_empty_next_cursor(api):
    api.answer(200, {"sandboxes": [SANDBOX]})

    page = api.client.list_sandboxes()

    assert page.next_cursor == ""


def test_iter_sandboxes_follows_the_cursor_until_the_last_page(api):
    api.answer(200, {"sandboxes": [SANDBOX], "next_cursor": "c2"})
    api.answer(200, {"sandboxes": [{**SANDBOX, "id": "sbx_2"}]})

    ids = [s.id for s in api.client.iter_sandboxes(states=["running"], page_size=1)]

    assert ids == ["sbx_1", "sbx_2"]
    assert [c.path for c in api.calls] == [
        "/v1/sandboxes?state=running&limit=1",
        "/v1/sandboxes?state=running&limit=1&cursor=c2",
    ]


def test_get_sandbox_returns_handle_with_info(api):
    api.answer(200, SANDBOX)

    sandbox = api.client.get_sandbox("sbx_1")

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1")
    assert sandbox.id == "sbx_1"
    assert sandbox.info.metadata == {"team": "search"}


def test_refresh_reads_the_sandbox_and_updates_info(api):
    sandbox = api.sandbox()
    api.answer(200, {**SANDBOX, "state": "paused"})

    info = sandbox.refresh()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1")
    assert info.state == "paused"
    assert sandbox.info is info


def test_set_timeout_posts_ttl_and_returns_new_expiry(api):
    api.answer(200, {**SANDBOX, "expires_at": "2026-11-20T10:20:00Z"})

    info = api.sandbox().set_timeout(600)

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/timeout")
    assert api.last.json == {"ttl_seconds": 600}
    assert info.expires_at == "2026-11-20T10:20:00Z"


def test_fork_sends_no_body_and_returns_the_new_sandbox(api):
    api.answer(201, {**SANDBOX, "id": "sbx_2"})

    fork = api.sandbox().fork()

    assert (api.last.method, api.last.path, api.last.body) == ("POST", "/v1/sandboxes/sbx_1/fork", None)
    assert fork.id == "sbx_2"
    assert fork.info.state == "running"


def test_fork_many_sends_count_and_returns_every_fork(api):
    api.answer(201, {"sandboxes": [{**SANDBOX, "id": "sbx_2"}, {**SANDBOX, "id": "sbx_3"}]})

    forks = api.sandbox().fork_many(2)

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/fork")
    assert api.last.json == {"count": 2}
    assert [f.id for f in forks] == ["sbx_2", "sbx_3"]


def test_pause_posts_to_pause_and_accepts_an_empty_answer(api):
    api.answer(200)

    result = api.sandbox().pause()

    assert (api.last.method, api.last.path, api.last.body) == ("POST", "/v1/sandboxes/sbx_1/pause", None)
    assert result is None


def test_resume_returns_the_resumed_sandbox_info(api):
    sandbox = api.sandbox()
    api.answer(200, SANDBOX)

    info = sandbox.resume()

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/resume")
    assert info.state == "running"
    assert sandbox.info is info


def test_destroy_deletes_the_sandbox(api):
    api.answer(204)

    api.sandbox().destroy()

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/sandboxes/sbx_1")


def test_metrics_decodes_resource_use(api):
    api.answer(200, {
        "cpu_usage_ns": 1500, "memory_used_bytes": 2048, "memory_limit_bytes": 4096, "at": "2026-11-20T10:00:01Z",
    })

    metrics = api.sandbox().metrics()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/metrics")
    assert metrics == SandboxMetrics(1500, 2048, 4096, "2026-11-20T10:00:01Z")


def test_logs_without_cursor_sends_no_query(api):
    api.answer(200, {"entries": [], "next": 0, "truncated": False})

    api.sandbox().logs()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/logs")


def test_logs_sends_cursor_and_long_poll_wait(api):
    api.answer(200, {"entries": [], "next": 12, "truncated": False})

    api.sandbox().logs(after=12, wait_ms=5000)

    assert api.last.path == "/v1/sandboxes/sbx_1/logs?after=12&wait_ms=5000"


def test_logs_decodes_entries_cursor_and_truncation(api):
    api.answer(200, {
        "entries": [{"pid": 7, "stream": "stderr", "data": "boom\n", "at": "2026-11-20T10:00:02Z"}],
        "next": 5,
        "truncated": True,
    })

    logs = api.sandbox().logs()

    assert isinstance(logs, SandboxLogs)
    assert (logs.next, logs.truncated) == (5, True)
    entry = logs.entries[0]
    assert (entry.pid, entry.stream, entry.data, entry.at) == (7, "stderr", "boom\n", "2026-11-20T10:00:02Z")
