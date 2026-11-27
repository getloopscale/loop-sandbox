"""Background processes."""
from loopsandbox import Process, ProcessOutput

RUNNING = {"pid": 7, "cmd": ["python3", "-m", "http.server"], "started_at": "2026-11-20T10:00:00Z",
           "running": True, "exit_code": None}
ENDED = {**RUNNING, "running": False, "exit_code": 143}


def test_start_process_sends_command_env_and_workdir(api):
    api.answer(201, RUNNING)

    api.sandbox().start_process(["python3", "-m", "http.server"], env={"PORT": "8000"}, workdir="/workspace/www")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/processes")
    assert api.last.json == {
        "cmd": ["python3", "-m", "http.server"], "env": {"PORT": "8000"}, "workdir": "/workspace/www",
    }


def test_start_process_returns_running_process_with_no_exit_code(api):
    api.answer(201, RUNNING)

    process = api.sandbox().start_process(["python3", "-m", "http.server"])

    assert process == Process(7, ["python3", "-m", "http.server"], "2026-11-20T10:00:00Z", True, None)


def test_list_processes_decodes_every_process(api):
    api.answer(200, {"processes": [RUNNING, {**ENDED, "pid": 8}]})

    processes = api.sandbox().list_processes()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/processes")
    assert [(p.pid, p.running, p.exit_code) for p in processes] == [(7, True, None), (8, False, 143)]


def test_get_process_reads_one_process_by_pid(api):
    api.answer(200, ENDED)

    process = api.sandbox().get_process(7)

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/processes/7")
    assert (process.running, process.exit_code) == (False, 143)


def test_process_output_from_the_start_sends_no_query(api):
    api.answer(200, {"stdout": "", "stderr": "", "next": 0, "truncated": False, "process": RUNNING})

    api.sandbox().process_output(7)

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/processes/7/output")


def test_process_output_sends_cursor_and_long_poll_wait(api):
    api.answer(200, {"stdout": "", "stderr": "", "next": 10, "truncated": False, "process": RUNNING})

    api.sandbox().process_output(7, after=10, wait_ms=2000)

    assert api.last.path == "/v1/sandboxes/sbx_1/processes/7/output?after=10&wait_ms=2000"


def test_process_output_decodes_streams_cursor_and_process_state(api):
    api.answer(200, {"stdout": "ready\n", "stderr": "warn\n", "next": 11, "truncated": True, "process": ENDED})

    output = api.sandbox().process_output(7)

    assert output == ProcessOutput(
        "ready\n", "warn\n", 11, True,
        Process(7, ["python3", "-m", "http.server"], "2026-11-20T10:00:00Z", False, 143),
    )


def test_write_process_stdin_posts_raw_bytes(api):
    api.answer(204)

    api.sandbox().write_process_stdin(7, b"hello\n")

    assert (api.last.method, api.last.path, api.last.body) == (
        "POST", "/v1/sandboxes/sbx_1/processes/7/stdin", b"hello\n",
    )
    assert api.last.headers["Content-Type"] == "application/octet-stream"


def test_close_process_stdin_deletes_the_stdin_route(api):
    api.answer(204)

    api.sandbox().close_process_stdin(7)

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/sandboxes/sbx_1/processes/7/stdin")


def test_signal_process_posts_the_signal_name(api):
    api.answer(204)

    api.sandbox().signal_process(7, "TERM")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/processes/7/signal")
    assert api.last.json == {"signal": "TERM"}
