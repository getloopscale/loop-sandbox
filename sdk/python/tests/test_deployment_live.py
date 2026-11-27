"""The SDK against a running LoopSandbox deployment.

Skipped unless LSB_BASE_URL and LSB_API_KEY are set. scripts/sdk-e2e.sh starts a
stack and sets them. Each test checks first that the deployment supports what it
uses, so the file passes against any driver.
"""

import os
import time

import pytest

from loopsandbox import LoopSandbox, NotFoundError, UnsupportedError

BASE_URL = os.environ.get("LSB_BASE_URL")
API_KEY = os.environ.get("LSB_API_KEY")
IMAGE = os.environ.get("LSB_IMAGE", "busybox:latest")

pytestmark = pytest.mark.skipif(not (BASE_URL and API_KEY), reason="LSB_BASE_URL and LSB_API_KEY are not set")


@pytest.fixture(scope="module")
def client():
    return LoopSandbox(API_KEY, BASE_URL)


@pytest.fixture(scope="module")
def supported(client):
    return {c.name for c in client.get_capabilities().capabilities if c.supported}


@pytest.fixture()
def sandbox(client):
    sb = client.create_sandbox(IMAGE, ttl_seconds=300)
    yield sb
    try:
        sb.destroy()
    except NotFoundError:
        pass  # the test destroyed it


def needs(supported, capability):
    if capability not in supported:
        pytest.skip(f"the deployment does not support {capability}")


def test_run_returns_the_exit_code_and_both_streams(sandbox):
    res = sandbox.run(["sh", "-c", "echo out; echo err >&2; exit 3"])

    assert (res.exit_code, res.stdout, res.stderr) == (3, "out\n", "err\n")


def test_list_sandboxes_shows_a_running_sandbox(client, sandbox):
    ids = [sb.id for sb in client.iter_sandboxes(states=["running"])]

    assert sandbox.id in ids


def test_a_destroyed_sandbox_is_not_found(client):
    sb = client.create_sandbox(IMAGE, ttl_seconds=300)
    sb.destroy()

    with pytest.raises(NotFoundError):
        sb.run(["true"])


def test_make_directory_move_and_file_info_agree(sandbox, supported):
    needs(supported, "file_ops")
    sandbox.make_directory("/workspace/a/b")
    sandbox.write_file("/workspace/a/b/x.txt", b"x")

    sandbox.move_file("/workspace/a/b/x.txt", "/workspace/a/y.txt")

    info = sandbox.file_info("/workspace/a/y.txt")
    assert (info.size, info.is_dir) == (1, False)


def test_a_background_process_reads_stdin_and_its_output_is_read_by_cursor(sandbox, supported):
    needs(supported, "processes")
    proc = sandbox.start_process(["sh", "-c", "read line; echo got:$line"])

    sandbox.write_process_stdin(proc.pid, b"ping\n")

    stdout, after, state = "", 0, proc
    deadline = time.monotonic() + 20
    while state.running and time.monotonic() < deadline:
        out = sandbox.process_output(proc.pid, after=after, wait_ms=2000)
        stdout, after, state = stdout + out.stdout, out.next, out.process
    assert "got:ping" in stdout
    assert state.exit_code == 0
    assert proc.pid in [p.pid for p in sandbox.list_processes()]


def test_a_watch_reports_a_written_file(sandbox, supported):
    needs(supported, "file_watch")
    watch = sandbox.start_watch("/workspace")

    sandbox.write_file("/workspace/watched.txt", b"w")

    events = sandbox.watch_events(watch.watch_id, wait_ms=5000)
    sandbox.stop_watch(watch.watch_id)
    assert any(e.path.endswith("watched.txt") for e in events.events)


def test_metrics_report_a_memory_limit(sandbox, supported):
    needs(supported, "metrics")

    metrics = sandbox.metrics()

    assert metrics.memory_limit_bytes > 0


def test_logs_carry_the_output_of_a_background_process(sandbox, supported):
    needs(supported, "logs")
    sandbox.start_process(["sh", "-c", "echo from-the-process"])

    data, after = "", 0
    deadline = time.monotonic() + 20
    while "from-the-process" not in data and time.monotonic() < deadline:
        page = sandbox.logs(after=after, wait_ms=2000)
        data, after = data + "".join(e.data for e in page.entries), page.next
    assert "from-the-process" in data


def test_a_file_uploaded_by_signed_url_is_downloaded_by_signed_url(client, sandbox):
    try:
        upload = sandbox.create_file_url("/workspace/up.txt", "upload", expires_in_seconds=60)
    except UnsupportedError:
        pytest.skip("signed URLs are off in this deployment")

    client.upload_signed_file(upload.url, b"by-url")

    download = sandbox.create_file_url("/workspace/up.txt", "download")
    assert client.download_signed_file(download.url) == b"by-url"
    assert sandbox.read_file("/workspace/up.txt") == b"by-url"


def test_a_session_keeps_variables_between_cells(client, supported):
    needs(supported, "sessions")
    sb = client.create_sandbox(os.environ.get("LSB_PYTHON_IMAGE", "python:3.12-slim"), ttl_seconds=300)
    try:
        session = sb.start_session()
        session.execute("x = 6 * 7")

        res = session.execute("print(x)")

        assert res.stdout.strip() == "42"
    finally:
        sb.destroy()


def test_fork_many_returns_independent_sandboxes(sandbox, supported):
    needs(supported, "fork")
    sandbox.write_file("/workspace/base.txt", b"base")

    forks = sandbox.fork_many(2)

    try:
        assert len(forks) == 2
        assert all(f.read_file("/workspace/base.txt") == b"base" for f in forks)
    finally:
        for f in forks:
            f.destroy()
