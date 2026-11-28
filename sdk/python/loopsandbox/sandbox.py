"""A Sandbox class with the shape of the Sandbox class of the e2b SDK.

A script written for e2b runs after two changes: the import, and
LOOPSCALE_API_KEY in place of E2B_API_KEY.
"""
from __future__ import annotations

import functools
import os
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Callable, Iterator, Mapping, Optional, Sequence, Union

from .client import LoopSandbox
from .client import Sandbox as SandboxHandle
from .errors import (
    CommandExitException,
    FileNotFoundException,
    LoopSandboxError,
    NotFoundError,
    SandboxNotFoundException,
    UnauthorizedError,
    ValidationError,
)
from .models import NetworkDenial, NetworkPolicy, Process
from .models import SandboxInfo as WireInfo

__all__ = [
    "ALL_TRAFFIC",
    "CommandHandle",
    "CommandResult",
    "EntryInfo",
    "FileType",
    "ProcessInfo",
    "Sandbox",
    "SandboxInfo",
    "SandboxMetrics",
    "SandboxPaginator",
    "WriteInfo",
]

DEFAULT_DOMAIN = "app.getloopscale.com"
# Every image ships /bin/sh; not every one ships bash.
SHELL = ["/bin/sh", "-c"]
# Where the files of a sandbox live. It is also where a command starts.
WORKSPACE = "/workspace"
# How long one poll for the output of a background command waits on the server.
OUTPUT_WAIT_MS = 10_000
# The destination that stands for every address in a deny rule.
ALL_TRAFFIC = "0.0.0.0/0"

Network = Union[NetworkPolicy, Mapping[str, Sequence[str]]]


class FileType(str, Enum):
    FILE = "file"
    DIR = "dir"


@dataclass
class WriteInfo:
    name: str
    path: str
    type: FileType


@dataclass
class EntryInfo(WriteInfo):
    size: int = 0
    mode: str = ""
    modified_time: Optional[datetime] = None


@dataclass
class CommandResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass
class ProcessInfo:
    pid: int
    cmd: str
    args: list[str]


@dataclass
class SandboxInfo:
    sandbox_id: str
    template_id: str  # the image or template the sandbox was created from
    state: str
    started_at: datetime
    end_at: datetime
    metadata: dict[str, str] = field(default_factory=dict)
    network: Optional[NetworkPolicy] = None


@dataclass
class SandboxMetrics:
    timestamp: datetime
    mem_used: int  # bytes
    mem_total: int  # bytes
    cpu_usage_ns: int  # CPU time used since the sandbox started
    disk_used: Optional[int] = None  # bytes in use in the workspace; None for a paused sandbox
    disk_total: Optional[int] = None  # size of the workspace in bytes


def _client(
    api_key: Optional[str] = None,
    domain: Optional[str] = None,
    api_url: Optional[str] = None,
    transport: Any = None,
    stream_transport: Any = None,
) -> LoopSandbox:
    key = api_key or os.environ.get("LOOPSCALE_API_KEY") or os.environ.get("LOOPSANDBOX_API_KEY")
    if not key:
        raise UnauthorizedError(
            0,
            "missing_api_key",
            "no API key: pass api_key or set LOOPSCALE_API_KEY. Create a key with the "
            "sandbox:execute scope under API Tokens in the console.",
        )
    base = api_url or os.environ.get("LOOPSCALE_API_URL")
    if not base:
        base = "https://" + (domain or os.environ.get("LOOPSCALE_DOMAIN") or DEFAULT_DOMAIN)
    return LoopSandbox(key, base, transport=transport, stream_transport=stream_transport)


@contextmanager
def _naming(specific: type) -> Iterator[None]:
    """Raise a plain 404 as the more specific error the caller's code catches."""
    try:
        yield
    except NotFoundError as e:
        if type(e) is NotFoundError:
            raise specific(e.status, e.code, e.message) from None
        raise


def _time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def _info(info: WireInfo) -> SandboxInfo:
    return SandboxInfo(
        sandbox_id=info.id,
        template_id=info.image,
        state=info.state,
        started_at=_time(info.created_at),
        end_at=_time(info.expires_at),
        metadata=dict(info.metadata),
        network=info.network,
    )


def _policy(network: Network) -> NetworkPolicy:
    return network if isinstance(network, NetworkPolicy) else NetworkPolicy.from_dict(network)


def _in_workspace(path: str) -> str:
    """Resolve a path the way a command in the sandbox would.

    A relative path starts at the workspace. A path outside the workspace is
    refused here, with the reason, because the API only reads and writes under it.
    """
    if not path.startswith("/"):
        relative = path[2:] if path.startswith("./") else path
        return WORKSPACE if relative in ("", ".") else f"{WORKSPACE}/{relative}"
    if path == WORKSPACE or path.startswith(WORKSPACE + "/"):
        return path
    raise ValidationError(
        0,
        "path_outside_workspace",
        f"the files of a sandbox live under {WORKSPACE}: use a relative path or one that starts "
        f"with {WORKSPACE} (got {path})",
    )


def _finished(result: CommandResult) -> CommandResult:
    if result.exit_code != 0:
        raise CommandExitException(result.exit_code, result.stdout, result.stderr)
    return result


class _on_class_or_object:
    """A method that is called as Sandbox.name(sandbox_id, ...) or sandbox.name(...)."""

    def __init__(self, fn: Callable[..., Any]):
        self._fn = fn
        functools.update_wrapper(self, fn)

    def __get__(self, obj: Any, cls: type) -> Callable[..., Any]:
        if obj is not None:
            return functools.partial(self._fn, obj._handle)

        def on_class(sandbox_id: str, *args: Any, **kwargs: Any) -> Any:
            conn = {k: kwargs.pop(k) for k in list(kwargs) if k in _CONNECTION}
            return self._fn(SandboxHandle(_client(**conn), sandbox_id), *args, **kwargs)

        return on_class


_CONNECTION = ("api_key", "domain", "api_url", "transport", "stream_transport")


class SandboxPaginator:
    """Pages through the sandboxes of the organization, newest first."""

    def __init__(self, client: LoopSandbox, query: Optional[Mapping[str, Any]], limit: Optional[int], next_token: Optional[str]):
        self._client, self._query, self._limit = client, query or {}, limit
        self._started = False
        self.next_token = next_token or None

    @property
    def has_next(self) -> bool:
        """True until the last page was read."""
        return not self._started or self.next_token is not None

    def next_items(self) -> list[SandboxInfo]:
        if not self.has_next:
            raise LoopSandboxError(0, "list_exhausted", "no more sandboxes to list")
        page = self._client.list_sandboxes(
            states=self._query.get("state"),
            metadata=self._query.get("metadata"),
            limit=self._limit,
            cursor=self.next_token or "",
        )
        self._started = True
        self.next_token = page.next_cursor or None
        return [_info(s.info) for s in page.sandboxes]


class CommandHandle:
    """A command started in the background."""

    def __init__(self, handle: SandboxHandle, process: Process, on_stdout: Any = None, on_stderr: Any = None):
        self._handle, self.pid = handle, process.pid
        self._on_stdout, self._on_stderr = on_stdout, on_stderr
        self._following = True

    def wait(self, on_stdout: Any = None, on_stderr: Any = None) -> CommandResult:
        """Wait for the command to end. Raises CommandExitException on a non-zero exit code."""
        on_stdout, on_stderr = on_stdout or self._on_stdout, on_stderr or self._on_stderr
        stdout, stderr, after = "", "", 0
        while self._following:
            out = self._handle.process_output(self.pid, after=after, wait_ms=OUTPUT_WAIT_MS)
            if out.stdout:
                stdout += out.stdout
                if on_stdout:
                    on_stdout(out.stdout)
            if out.stderr:
                stderr += out.stderr
                if on_stderr:
                    on_stderr(out.stderr)
            after = out.next
            if not out.process.running:
                code = out.process.exit_code
                return _finished(CommandResult(-1 if code is None else code, stdout, stderr))
        raise LoopSandboxError(0, "command_disconnected", "disconnected from the command before it ended")

    def kill(self) -> bool:
        return _kill_process(self._handle, self.pid)

    def disconnect(self) -> None:
        """Stop following the command, which keeps running."""
        self._following = False


def _kill_process(handle: SandboxHandle, pid: int) -> bool:
    try:
        handle.signal_process(pid, "KILL")
    except NotFoundError:
        return False
    return True


class Commands:
    """Commands of one sandbox, with the method names of the e2b SDK."""

    def __init__(self, handle: SandboxHandle):
        self._handle = handle

    def run(
        self,
        cmd: str,
        background: Optional[bool] = None,
        envs: Optional[Mapping[str, str]] = None,
        cwd: Optional[str] = None,
        on_stdout: Optional[Callable[[str], Any]] = None,
        on_stderr: Optional[Callable[[str], Any]] = None,
        timeout: Optional[float] = None,
    ) -> Union[CommandResult, CommandHandle]:
        """Run a command line through /bin/sh.

        background=True answers a CommandHandle at once. timeout is in seconds.
        Raises CommandExitException when the command exits with a non-zero code.
        """
        argv = [*SHELL, cmd]
        workdir = _in_workspace(cwd) if cwd else ""
        if background:
            process = self._handle.start_process(argv, env=envs, workdir=workdir)
            return CommandHandle(self._handle, process, on_stdout, on_stderr)
        timeout_ms = int(timeout * 1000) if timeout else 0
        if not on_stdout and not on_stderr:
            r = self._handle.run(argv, workdir, env=envs, timeout_ms=timeout_ms)
            return _finished(CommandResult(r.exit_code, r.stdout, r.stderr))
        stdout, stderr = "", ""
        for event in self._handle.run_stream(argv, workdir, env=envs, timeout_ms=timeout_ms):
            if event.type == "stdout":
                stdout += event.data
                if on_stdout:
                    on_stdout(event.data)
            elif event.type == "stderr":
                stderr += event.data
                if on_stderr:
                    on_stderr(event.data)
            elif event.type == "done":
                return _finished(CommandResult(event.exit_code or 0, stdout, stderr))
        raise LoopSandboxError(0, "stream_incomplete", "the command stream ended without an exit code")

    def connect(self, pid: int, on_stdout: Any = None, on_stderr: Any = None) -> CommandHandle:
        """Follow a command started in the background by its pid."""
        return CommandHandle(self._handle, self._handle.get_process(pid), on_stdout, on_stderr)

    def list(self) -> list[ProcessInfo]:
        """The background commands of the sandbox, running or ended."""
        return [ProcessInfo(p.pid, p.cmd[0] if p.cmd else "", list(p.cmd[1:])) for p in self._handle.list_processes()]

    def kill(self, pid: int) -> bool:
        """Kill a background command. False when no command has that pid."""
        return _kill_process(self._handle, pid)

    def send_stdin(self, pid: int, data: Union[str, bytes]) -> None:
        self._handle.write_process_stdin(pid, data.encode() if isinstance(data, str) else data)

    def close_stdin(self, pid: int) -> None:
        self._handle.close_process_stdin(pid)


class Filesystem:
    """Files of one sandbox, with the method names of the e2b SDK."""

    def __init__(self, handle: SandboxHandle):
        self._handle = handle

    def read(self, path: str, format: str = "text") -> Union[str, bytes]:  # noqa: A002 - the name e2b uses
        with _naming(FileNotFoundException):
            data = self._handle.read_file(_in_workspace(path))
        if format == "bytes":
            return data
        if format == "text":
            return data.decode()
        raise ValidationError(0, "invalid_argument", f'format must be "text" or "bytes" (got {format!r})')

    def write(self, path: str, data: Union[str, bytes]) -> WriteInfo:
        target = _in_workspace(path)
        self._handle.write_file(target, data.encode() if isinstance(data, str) else data)
        return WriteInfo(target.rstrip("/").rsplit("/", 1)[-1], target, FileType.FILE)

    def write_files(self, files: Sequence[Mapping[str, Any]]) -> list[WriteInfo]:
        """Write several files. Each entry has "path" and "data"."""
        return [self.write(f["path"], f["data"]) for f in files]

    def list(self, path: str) -> list[EntryInfo]:
        """The entries directly under a directory."""
        directory = _in_workspace(path)
        with _naming(FileNotFoundException):
            entries = self._handle.list_files(directory)
        return [
            EntryInfo(e.name, f"{directory.rstrip('/')}/{e.name}", FileType.DIR if e.is_dir else FileType.FILE, e.size)
            for e in entries
        ]

    def exists(self, path: str) -> bool:
        try:
            self._handle.file_info(_in_workspace(path))
        except NotFoundError:
            return False
        return True

    def get_info(self, path: str) -> EntryInfo:
        with _naming(FileNotFoundException):
            i = self._handle.file_info(_in_workspace(path))
        return EntryInfo(i.name, i.path, FileType.DIR if i.is_dir else FileType.FILE, i.size, i.mode, _time(i.modified_at))

    def make_dir(self, path: str) -> bool:
        """Create a directory and its missing parents."""
        self._handle.make_directory(_in_workspace(path))
        return True

    def remove(self, path: str) -> None:
        with _naming(FileNotFoundException):
            self._handle.delete_file(_in_workspace(path))

    def rename(self, old_path: str, new_path: str) -> EntryInfo:
        target = _in_workspace(new_path)
        with _naming(FileNotFoundException):
            self._handle.move_file(_in_workspace(old_path), target)
        return self.get_info(target)


class Sandbox:
    """A sandbox.

        sandbox = Sandbox.create()            # reads LOOPSCALE_API_KEY
        sandbox.files.write("hi.txt", "hello")
        print(sandbox.commands.run("cat hi.txt").stdout)
        sandbox.kill()
    """

    def __init__(self, handle: SandboxHandle):
        self._handle = handle
        self.sandbox_id = handle.id
        self.commands = Commands(handle)
        self.files = Filesystem(handle)

    @classmethod
    def create(
        cls,
        template: Optional[str] = None,
        timeout: Optional[int] = None,
        metadata: Optional[Mapping[str, str]] = None,
        envs: Optional[Mapping[str, str]] = None,
        allow_internet_access: bool = True,
        network: Optional[Network] = None,
        secrets: Optional[Sequence[str]] = None,
        **connection: Any,
    ) -> "Sandbox":
        """Create a sandbox.

        template is an image ("python:3.12-slim") when it has a ":" or a "/", and
        the name of a template otherwise. timeout is the lifetime in seconds.
        allow_internet_access=False denies every outbound destination; network
        wins over it.
        """
        is_image = bool(template) and (":" in template or "/" in template)
        rules = _policy(network) if network is not None else None
        if rules is None and not allow_internet_access:
            rules = NetworkPolicy(allow_out=[], deny_out=[ALL_TRAFFIC])
        handle = _client(**connection).create_sandbox(
            image=template if is_image else "",
            template="" if is_image or not template else template,
            ttl_seconds=timeout or 0,
            metadata=metadata,
            env=envs,
            network=rules,
            secrets=secrets,
        )
        return cls(handle)

    @classmethod
    def connect(cls, sandbox_id: str, **connection: Any) -> "Sandbox":
        """Connect to a sandbox that exists, resuming it when it is paused."""
        with _naming(SandboxNotFoundException):
            handle = _client(**connection).get_sandbox(sandbox_id)
            if handle.info is not None and handle.info.state == "paused":
                handle.resume()
        return cls(handle)

    @staticmethod
    def list(
        query: Optional[Mapping[str, Any]] = None,
        limit: Optional[int] = None,
        next_token: Optional[str] = None,
        **connection: Any,
    ) -> SandboxPaginator:
        """List sandboxes. query takes "state" (a list) and "metadata" (a dict)."""
        return SandboxPaginator(_client(**connection), query, limit, next_token)

    @_on_class_or_object
    def kill(handle: SandboxHandle) -> bool:  # noqa: N805
        """Kill the sandbox. False when it no longer exists."""
        try:
            handle.destroy()
        except NotFoundError:
            return False
        return True

    @_on_class_or_object
    def is_running(handle: SandboxHandle) -> bool:  # noqa: N805
        """True while the sandbox exists and is running."""
        try:
            return handle.refresh().state == "running"
        except NotFoundError:
            return False

    @_on_class_or_object
    def get_info(handle: SandboxHandle) -> SandboxInfo:  # noqa: N805
        with _naming(SandboxNotFoundException):
            return _info(handle.refresh())

    @_on_class_or_object
    def get_metrics(handle: SandboxHandle) -> list[SandboxMetrics]:  # noqa: N805
        """The current use of CPU and memory, as a list of one sample."""
        with _naming(SandboxNotFoundException):
            m = handle.metrics()
        return [
            SandboxMetrics(
                _time(m.at), m.memory_used_bytes, m.memory_limit_bytes, m.cpu_usage_ns,
                m.disk_used_bytes, m.disk_limit_bytes,
            )
        ]

    @_on_class_or_object
    def pause(handle: SandboxHandle) -> bool:  # noqa: N805
        """Pause the sandbox. Sandbox.connect resumes it."""
        with _naming(SandboxNotFoundException):
            handle.pause()
        return True

    @_on_class_or_object
    def set_timeout(handle: SandboxHandle, timeout: int) -> None:  # noqa: N805
        """Make the sandbox expire timeout seconds from now."""
        with _naming(SandboxNotFoundException):
            handle.set_timeout(timeout)

    @_on_class_or_object
    def update_network(handle: SandboxHandle, network: Network) -> NetworkPolicy:  # noqa: N805
        """Replace the outbound rules of the running sandbox."""
        rules = _policy(network)
        with _naming(SandboxNotFoundException):
            return handle.update_network(rules.allow_out, rules.deny_out)

    @_on_class_or_object
    def network_denials(handle: SandboxHandle, limit: int = 0) -> list[NetworkDenial]:  # noqa: N805
        """The outbound connections the sandbox was refused. Not in the e2b SDK."""
        with _naming(SandboxNotFoundException):
            return handle.network_denials(limit)
