"""REAL cross-SDK contract test: the Python SDK against the LIVE Go API router (fake
driver with a real python kernel). Proves the Python SDK produces the same behavior the TS
SDK is tested for — the M14 parity DoD. Spawns `go run ./testserver`."""
import os
import shutil
import socket
import subprocess
import time

import pytest

from loopsandbox import LoopSandbox, NotFoundError

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


def _free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


@pytest.fixture(scope="module")
def live_api():
    if shutil.which("go") is None or shutil.which("python3") is None:
        pytest.skip("go or python3 not available")
    port = _free_port()
    env = {**os.environ, "ADDR": f"127.0.0.1:{port}"}
    proc = subprocess.Popen(
        ["go", "run", "./sdk/python/testserver"],
        cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    base = f"http://127.0.0.1:{port}"
    # wait for readiness
    ok = False
    for _ in range(120):
        try:
            import urllib.request
            urllib.request.urlopen(base + "/v1/healthz", timeout=1)
            ok = True
            break
        except Exception:
            time.sleep(0.5)
    if not ok:
        proc.terminate()
        pytest.skip("test API server did not start")
    yield base
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except Exception:
        proc.kill()


def test_python_sdk_state_sharing_against_live_api(live_api):
    c = LoopSandbox("lsb_test_key", live_api)
    sbx = c.create_sandbox(image="python")
    # run_code shares state across calls (real python kernel behind the live API)
    r1 = sbx.run_code("x = 41")
    assert r1.exit_code == 0
    r2 = sbx.run_code("print(x + 1)")
    assert r2.stdout.strip() == "42", f"state not shared: {r2}"
    sbx.destroy()


def test_python_sdk_fork_against_live_api(live_api):
    c = LoopSandbox("lsb_test_key", live_api)
    sbx = c.create_sandbox()
    sbx.write_file("/workspace/a.txt", b"parent")
    fork = sbx.fork()
    assert fork.read_file("/workspace/a.txt") == b"parent"
    # write to fork -> parent unaffected
    fork.write_file("/workspace/b.txt", b"fork")
    try:
        sbx.read_file("/workspace/b.txt")
        assert False, "parent saw fork's file"
    except NotFoundError:
        pass


def test_python_sdk_cross_tenant_is_404(live_api):
    owner = LoopSandbox("lsb_test_key", live_api)
    other = LoopSandbox("lsb_other_key", live_api)
    sbx = owner.create_sandbox()
    try:
        other.run_code_sandbox = None  # noqa
        Sandbox = type(sbx)
        Sandbox(other, sbx.id).run(["echo", "hi"])
        assert False, "cross-tenant exec succeeded"
    except NotFoundError:
        pass
