"""LoopSandbox in-guest code-interpreter kernel (M12).

A persistent Python interpreter: reads length-prefixed JSON {"code": ...} frames from
stdin, executes each in a SHARED namespace (so state — variables, imports, dataframes —
survives across cells), captures stdout/stderr, and writes a length-prefixed JSON
{"stdout","stderr","error"} frame to stdout. Framing matches internal/agentproto
(4-byte big-endian length + JSON body).

Runs INSIDE the sandbox guest (untrusted code); it is spawned + supervised by the agent.
"""
import sys
import io
import json
import struct
import contextlib
import traceback

_MAX = 8 << 20  # cap captured output (matches agent maxOutput) so a cell cannot OOM


def _read_frame(f):
    hdr = f.read(4)
    if len(hdr) < 4:
        return None
    n = struct.unpack(">I", hdr)[0]
    body = f.read(n)
    if len(body) < n:
        return None
    return json.loads(body)


def _write_frame(f, obj):
    body = json.dumps(obj).encode("utf-8")
    f.write(struct.pack(">I", len(body)))
    f.write(body)
    f.flush()


class _StreamWriter(io.TextIOBase):
    """Emits an output frame per write so the host sees partial stdout AS THE CELL RUNS."""

    def __init__(self, out, which):
        self._out = out
        self._which = which

    def write(self, s):
        if s:
            _write_frame(self._out, {"type": "out", "stream": self._which, "data": s[:_MAX]})
        return len(s)


def main():
    ns = {"__name__": "__main__"}  # persistent shared namespace across cells
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    while True:
        req = _read_frame(stdin)
        if req is None:
            return
        code_str = req.get("code", "")
        if req.get("stream"):
            # Streaming mode: emit an "out" frame per write, then a "done" frame.
            error = ""
            errbuf = io.StringIO()
            try:
                code = compile(code_str, "<cell>", "exec")
                with contextlib.redirect_stdout(_StreamWriter(stdout, "stdout")), \
                        contextlib.redirect_stderr(errbuf):
                    exec(code, ns)  # noqa: S102
            except BaseException:  # noqa: BLE001
                error = traceback.format_exc()
            _write_frame(stdout, {"type": "done", "stderr": errbuf.getvalue()[:_MAX], "error": error[:_MAX]})
            continue
        # Non-streaming (default): one result frame with the full output.
        out, err = io.StringIO(), io.StringIO()
        error = ""
        try:
            code = compile(code_str, "<cell>", "exec")
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                exec(code, ns)  # noqa: S102 — untrusted-by-design, isolated in the guest
        except BaseException:  # noqa: BLE001 — a cell may raise anything; report, never die
            error = traceback.format_exc()
        _write_frame(stdout, {
            "stdout": out.getvalue()[:_MAX],
            "stderr": err.getvalue()[:_MAX],
            "error": error[:_MAX],
        })


if __name__ == "__main__":
    main()
