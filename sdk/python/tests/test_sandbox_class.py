"""The Sandbox class, which has the shape of the Sandbox class of the e2b SDK."""
import json

import pytest

from loopsandbox import (
    AuthenticationException,
    CommandExitException,
    FileNotFoundException,
    FileType,
    InvalidArgumentException,
    Sandbox,
    SandboxException,
    StreamResponse,
)

WIRE = {
    "id": "sbx_1",
    "tenant_id": "tenant-a",
    "image": "python:3.12-slim",
    "state": "running",
    "created_at": "2026-11-28T10:00:00Z",
    "expires_at": "2026-11-28T10:05:00Z",
    "metadata": {"app": "demo"},
}


def conn(api, **extra):
    return {"api_key": "theo_live_k", "api_url": "https://api.test", "transport": api._transport, **extra}


def created(api, **extra):
    api.answer(201, WIRE)
    return Sandbox.create(**conn(api, **extra))


def test_create_reads_the_key_and_the_domain_from_the_environment(api, monkeypatch):
    monkeypatch.setenv("LOOPSCALE_API_KEY", "theo_live_env")
    monkeypatch.setenv("LOOPSCALE_DOMAIN", "app.staging.example")
    seen = []

    def transport(method, url, headers, body):
        seen.append((url, headers))
        return api._transport(method, url, headers, body)

    api.answer(201, WIRE)
    sandbox = Sandbox.create(transport=transport)

    assert sandbox.sandbox_id == "sbx_1"
    assert seen[0][0] == "https://app.staging.example/v1/sandboxes"
    assert seen[0][1]["Authorization"] == "Bearer theo_live_env"


def test_create_without_a_key_says_which_variable_to_set(api, monkeypatch):
    monkeypatch.delenv("LOOPSCALE_API_KEY", raising=False)
    monkeypatch.delenv("LOOPSANDBOX_API_KEY", raising=False)

    with pytest.raises(AuthenticationException, match="LOOPSCALE_API_KEY"):
        Sandbox.create(transport=api._transport)
    assert api.calls == []


def test_create_sends_the_timeout_in_seconds_the_envs_and_the_rules(api):
    api.answer(201, WIRE)

    Sandbox.create(
        "python:3.12-slim",
        timeout=90,
        metadata={"app": "demo"},
        envs={"A": "1"},
        network={"allow_out": ["pypi.org"], "deny_out": ["0.0.0.0/0"]},
        **conn(api),
    )

    assert api.last.json == {
        "image": "python:3.12-slim",
        "ttl_seconds": 90,
        "metadata": {"app": "demo"},
        "env": {"A": "1"},
        "network": {"allow_out": ["pypi.org"], "deny_out": ["0.0.0.0/0"]},
    }


def test_create_takes_a_name_without_colon_or_slash_as_a_template(api):
    api.answer(201, WIRE)

    Sandbox.create("data-science", **conn(api))

    assert api.last.json == {"template": "data-science"}


def test_create_without_internet_access_denies_every_destination(api):
    api.answer(201, WIRE)

    Sandbox.create(allow_internet_access=False, **conn(api))

    assert api.last.json["network"] == {"allow_out": [], "deny_out": ["0.0.0.0/0"]}


def test_connect_resumes_a_paused_sandbox(api):
    api.answer(200, {**WIRE, "state": "paused"})
    api.answer(200, WIRE)

    sandbox = Sandbox.connect("sbx_1", **conn(api))

    assert sandbox.sandbox_id == "sbx_1"
    assert [(c.method, c.path) for c in api.calls] == [
        ("GET", "/v1/sandboxes/sbx_1"),
        ("POST", "/v1/sandboxes/sbx_1/resume"),
    ]


def test_kill_works_on_the_class_and_on_the_object(api):
    sandbox = created(api)
    api.answer(204)
    api.error(404, "not_found", "sandbox not found")

    assert sandbox.kill() is True
    assert Sandbox.kill("sbx_1", **conn(api)) is False


def test_get_info_uses_the_field_names_of_e2b(api):
    api.answer(200, {**WIRE, "network": {"allow_out": ["pypi.org"], "deny_out": []}})

    info = Sandbox.get_info("sbx_1", **conn(api))

    assert (info.sandbox_id, info.template_id, info.state, info.metadata) == (
        "sbx_1", "python:3.12-slim", "running", {"app": "demo"},
    )
    assert info.started_at.isoformat() == "2026-11-28T10:00:00+00:00"
    assert info.end_at.isoformat() == "2026-11-28T10:05:00+00:00"


def test_get_metrics_carries_the_use_of_the_workspace_when_the_api_reports_it(api):
    sample = {"cpu_usage_ns": 5, "memory_used_bytes": 100, "memory_limit_bytes": 400, "at": "2026-11-28T10:01:00Z"}
    api.answer(200, {**sample, "disk_used_bytes": 4096, "disk_limit_bytes": 65536})
    api.answer(200, sample)

    with_disk = Sandbox.get_metrics("sbx_1", **conn(api))[0]
    without_disk = Sandbox.get_metrics("sbx_1", **conn(api))[0]

    assert (with_disk.mem_used, with_disk.mem_total, with_disk.disk_used, with_disk.disk_total) == (100, 400, 4096, 65536)
    assert (without_disk.disk_used, without_disk.disk_total) == (None, None)


def test_is_running_is_false_for_a_paused_and_for_a_missing_sandbox(api):
    sandbox = created(api)
    api.answer(200, WIRE)
    api.answer(200, {**WIRE, "state": "paused"})
    api.error(404, "not_found", "sandbox not found")

    assert [sandbox.is_running(), sandbox.is_running(), sandbox.is_running()] == [True, False, False]


def test_set_timeout_takes_seconds(api):
    sandbox = created(api)
    api.answer(200, WIRE)

    sandbox.set_timeout(120)

    assert (api.last.path, api.last.json) == ("/v1/sandboxes/sbx_1/timeout", {"ttl_seconds": 120})


def test_list_pages_through_the_sandboxes(api):
    api.answer(200, {"sandboxes": [WIRE], "next_cursor": "c1"})
    api.answer(200, {"sandboxes": [{**WIRE, "id": "sbx_2"}]})

    paginator = Sandbox.list(query={"state": ["running"]}, limit=1, **conn(api))
    seen = []
    while paginator.has_next:
        seen += [info.sandbox_id for info in paginator.next_items()]

    assert seen == ["sbx_1", "sbx_2"]
    assert "state=running" in api.calls[0].path and "cursor=c1" in api.calls[1].path


def test_run_sends_the_command_line_through_the_shell(api):
    sandbox = created(api)
    api.answer(200, {"exit_code": 0, "stdout": "hi\n", "stderr": ""})

    result = sandbox.commands.run("echo hi", cwd="src", envs={"A": "1"}, timeout=5)

    assert (result.exit_code, result.stdout, result.stderr) == (0, "hi\n", "")
    assert api.last.json == {
        "cmd": ["/bin/sh", "-c", "echo hi"],
        "workdir": "/workspace/src",
        "env": {"A": "1"},
        "timeout_ms": 5000,
    }


def test_run_raises_with_the_output_when_the_command_fails(api):
    sandbox = created(api)
    api.answer(200, {"exit_code": 3, "stdout": "out", "stderr": "boom"})

    with pytest.raises(CommandExitException) as raised:
        sandbox.commands.run("exit 3")

    assert isinstance(raised.value, SandboxException)
    assert (raised.value.exit_code, raised.value.stdout, raised.value.stderr) == (3, "out", "boom")


def test_run_delivers_output_to_the_callbacks_as_it_arrives(api):
    lines = []
    for name, data in [("stdout", {"data": "a"}), ("stderr", {"data": "b"}), ("stdout", {"data": "c"}), ("done", {"exit_code": 0})]:
        lines += [f"event: {name}\n".encode(), f"data: {json.dumps(data)}\n".encode(), b"\n"]
    sandbox = created(api, stream_transport=lambda *_: StreamResponse(200, lines, {}))
    out, err = [], []

    result = sandbox.commands.run("make", on_stdout=out.append, on_stderr=err.append)

    assert (out, err) == (["a", "c"], ["b"])
    assert (result.exit_code, result.stdout, result.stderr) == (0, "ac", "b")


def test_a_background_command_answers_through_its_handle(api):
    sandbox = created(api)
    proc = {"pid": 7, "cmd": ["/bin/sh", "-c", "sleep 1"], "started_at": "2026-11-28T10:00:01Z", "running": True, "exit_code": None}
    api.answer(201, proc)
    api.answer(200, {"stdout": "do", "stderr": "", "next": 2, "truncated": False, "process": proc})
    api.answer(200, {"stdout": "ne", "stderr": "", "next": 4, "truncated": False, "process": {**proc, "running": False, "exit_code": 0}})

    handle = sandbox.commands.run("sleep 1", background=True)
    result = handle.wait()

    assert handle.pid == 7
    assert (result.exit_code, result.stdout) == (0, "done")


def test_files_write_resolves_a_relative_path_against_the_workspace(api):
    sandbox = created(api)
    api.answer(204)

    written = sandbox.files.write("notes/a.txt", "hello")

    assert (api.last.method, api.last.path, api.last.body) == ("PUT", "/v1/sandboxes/sbx_1/files/workspace/notes/a.txt", b"hello")
    assert (written.name, written.path, written.type) == ("a.txt", "/workspace/notes/a.txt", FileType.FILE)


def test_files_refuse_a_path_outside_the_workspace_and_say_where_files_live(api):
    sandbox = created(api)

    with pytest.raises(InvalidArgumentException, match=r"under /workspace.*/home/user/a\.txt"):
        sandbox.files.write("/home/user/a.txt", "hello")
    assert len(api.calls) == 1


def test_files_read_answers_text_by_default_and_bytes_when_asked(api):
    sandbox = created(api)
    api.answer(200, b"hello")
    api.answer(200, b"hello")

    assert sandbox.files.read("a.txt") == "hello"
    assert sandbox.files.read("a.txt", format="bytes") == b"hello"


def test_files_exists_is_false_for_a_missing_path(api):
    sandbox = created(api)
    api.error(404, "not_found", "no such file")

    assert sandbox.files.exists("b.txt") is False


def test_files_list_answers_entries_with_a_type_and_a_full_path(api):
    sandbox = created(api)
    api.answer(200, {"files": [{"name": "a.txt", "size": 5, "is_dir": False}, {"name": "src", "size": 0, "is_dir": True}]})

    entries = sandbox.files.list(".")

    assert [(e.name, e.path, e.type, e.size) for e in entries] == [
        ("a.txt", "/workspace/a.txt", FileType.FILE, 5),
        ("src", "/workspace/src", FileType.DIR, 0),
    ]


def test_files_remove_of_a_missing_file_raises_file_not_found(api):
    sandbox = created(api)
    api.error(404, "not_found", "no such file")

    with pytest.raises(FileNotFoundException):
        sandbox.files.remove("b.txt")
