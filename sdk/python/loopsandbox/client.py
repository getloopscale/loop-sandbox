"""The LoopSandbox client and the handles it returns.

LoopSandbox holds the operations of the tenant (sandboxes, volumes, templates, API
keys, platform). Sandbox holds the operations of one sandbox, and Session those of
one code session.
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Iterator, Mapping, Optional, Sequence
from urllib.parse import quote, urlencode

from .errors import StreamError, error_from_response
from .models import (
    ApiKey,
    ArtifactUrl,
    Capabilities,
    CellResult,
    ExecResult,
    FileEntry,
    FileEvents,
    FileInfo,
    Process,
    ProcessOutput,
    SandboxInfo,
    SandboxLogs,
    SandboxMetrics,
    SignedFileUrl,
    StreamEvent,
    Template,
    Secret,
    Snapshot,
    Volume,
    Watch,
)
from .transport import (
    Response,
    StreamTransport,
    Transport,
    default_stream_transport,
    default_transport,
)

__all__ = ["LoopSandbox", "Sandbox", "SandboxPage", "Session"]

_OCTET_STREAM = "application/octet-stream"
_SIGNED_FILES_PATH = "/v1/signed-files/"


def _segment(value: Any) -> str:
    """Encode one path segment, slashes included."""
    return quote(str(value), safe="")


def _file_path(path: str) -> str:
    """Encode a file path for the URL. The API takes its slashes unencoded."""
    return quote(path.lstrip("/"), safe="/")


def _cursor_query(after: int, wait_ms: int) -> dict:
    query: dict = {}
    if after:
        query["after"] = after
    if wait_ms:
        query["wait_ms"] = wait_ms
    return query


def _parse_sse(lines: Any) -> Iterator[tuple[str, dict]]:
    """Yield (event name, decoded data) for each Server-Sent Event in lines."""
    event, data = "", []
    for raw in lines:
        line = raw.decode().rstrip("\r\n")
        if line.startswith("event:"):
            event = line[len("event:"):].strip()
        elif line.startswith("data:"):
            data.append(line[len("data:"):].lstrip(" "))
        elif line == "" and (event or data):
            yield event, json.loads("\n".join(data)) if data else {}
            event, data = "", []


class LoopSandbox:
    """Client. api_key authenticates; base_url points at the /v1 API host."""

    def __init__(
        self,
        api_key: str,
        base_url: str,
        transport: Optional[Transport] = None,
        stream_transport: Optional[StreamTransport] = None,
    ):
        self._key = api_key
        self._base = base_url.rstrip("/")
        self._transport = transport or default_transport
        self._stream_transport = stream_transport or default_stream_transport

    # --- HTTP plumbing --------------------------------------------------------------------

    def _url(self, path: str, query: Optional[Mapping[str, Any]] = None) -> str:
        url = self._base + path
        if query:
            url += "?" + urlencode(query, doseq=True)
        return url

    def _headers(self, auth: bool, content_type: Optional[str], extra: Optional[Mapping[str, str]]) -> dict:
        headers: dict = {}
        if auth:
            headers["Authorization"] = f"Bearer {self._key}"
        if content_type:
            headers["Content-Type"] = content_type
        if extra:
            headers.update(extra)
        return headers

    def _request(
        self,
        method: str,
        path: str,
        *,
        query: Optional[Mapping[str, Any]] = None,
        json_body: Optional[Any] = None,
        data: Optional[bytes] = None,
        headers: Optional[Mapping[str, str]] = None,
        auth: bool = True,
        url: Optional[str] = None,
    ) -> Response:
        """Send one request and raise the typed error of a failed answer.

        json_body is sent as JSON and data as raw bytes. url replaces the address
        built from path, for a signed URL.
        """
        content_type = None
        if json_body is not None:
            data = json.dumps(json_body).encode()
            content_type = "application/json"
        elif data is not None:
            content_type = _OCTET_STREAM
        resp = self._transport(method, url or self._url(path, query), self._headers(auth, content_type, headers), data)
        if resp.status >= 400:
            raise error_from_response(resp.status, resp.body, resp.headers)
        return resp

    def _do(self, method: str, path: str, body: Optional[dict] = None) -> Response:
        return self._request(method, path, json_body=body)

    def _stream(self, path: str, json_body: Any) -> Iterator[StreamEvent]:
        """POST with stream=1 and yield the events until "done"."""
        headers = self._headers(True, "application/json", {"Accept": "text/event-stream"})
        resp = self._stream_transport("POST", self._url(path, {"stream": "1"}), headers, json.dumps(json_body).encode())
        try:
            if resp.status >= 400:
                raise error_from_response(resp.status, b"".join(resp.lines), resp.headers)
            for event, payload in _parse_sse(resp.lines):
                if event == "error":
                    raise StreamError(resp.status, str(payload.get("code") or "error"), str(payload.get("error") or ""))
                if event == "done":
                    yield StreamEvent("done", exit_code=payload["exit_code"], stderr=payload.get("stderr", ""))
                    return
                yield StreamEvent(event, data=payload.get("data", ""))
            raise StreamError(resp.status, "stream_interrupted", "the stream ended before its done event")
        finally:
            if resp.close is not None:
                resp.close()

    # --- sandboxes ------------------------------------------------------------------------

    def create_sandbox(
        self,
        image: str = "",
        ttl_seconds: int = 0,
        *,
        template: str = "",
        snapshot: str = "",
        auto_pause_seconds: int = 0,
        secrets: Optional[Sequence[str]] = None,
        gpu: str = "",
        metadata: Optional[Mapping[str, str]] = None,
        env: Optional[Mapping[str, str]] = None,
        idempotency_key: str = "",
    ) -> "Sandbox":
        """Create a sandbox. template and gpu are mutually exclusive.

        snapshot is the id of a snapshot to start from, and cannot be combined
        with image, template or gpu.

        auto_pause_seconds pauses the sandbox after that many seconds without a
        request that uses it, and the next one resumes it. It needs the
        capability "pause".

        secrets names the tenant's secrets that the commands and background
        processes of the sandbox receive as environment variables.

        With idempotency_key, the same key and arguments replay the first result
        instead of creating a second sandbox.
        """
        body: dict = {}
        if image:
            body["image"] = image
        if template:
            body["template"] = template
        if snapshot:
            body["snapshot"] = snapshot
        if gpu:
            body["gpu"] = gpu
        if ttl_seconds:
            body["ttl_seconds"] = ttl_seconds
        if auto_pause_seconds:
            body["auto_pause_seconds"] = auto_pause_seconds
        if secrets:
            body["secrets"] = list(secrets)
        if metadata:
            body["metadata"] = dict(metadata)
        if env:
            body["env"] = dict(env)
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        info = self._request("POST", "/v1/sandboxes", json_body=body, headers=headers).json()
        return Sandbox(self, info["id"], SandboxInfo.from_dict(info))

    def list_sandboxes(
        self,
        *,
        states: Optional[Sequence[str]] = None,
        metadata: Optional[Mapping[str, str]] = None,
        limit: Optional[int] = None,
        cursor: str = "",
    ) -> "SandboxPage":
        """Return one page of sandboxes, newest first.

        states keeps sandboxes in one of the given states; metadata keeps those that
        carry every given label. Send the next_cursor of a page as cursor, with the
        same filters, to get the following page.
        """
        query: dict = {}
        if states:
            query["state"] = ",".join(states)
        if metadata:
            query["metadata"] = [f"{key}={value}" for key, value in metadata.items()]
        if limit is not None:
            query["limit"] = limit
        if cursor:
            query["cursor"] = cursor
        page = self._request("GET", "/v1/sandboxes", query=query).json()
        sandboxes = [Sandbox(self, s["id"], SandboxInfo.from_dict(s)) for s in page.get("sandboxes") or []]
        return SandboxPage(sandboxes, page.get("next_cursor", ""))

    def iter_sandboxes(
        self,
        *,
        states: Optional[Sequence[str]] = None,
        metadata: Optional[Mapping[str, str]] = None,
        page_size: Optional[int] = None,
    ) -> Iterator["Sandbox"]:
        """Yield every sandbox that matches, following the pages of list_sandboxes."""
        cursor = ""
        while True:
            page = self.list_sandboxes(states=states, metadata=metadata, limit=page_size, cursor=cursor)
            yield from page.sandboxes
            if not page.next_cursor:
                return
            cursor = page.next_cursor

    def get_sandbox(self, sandbox_id: str) -> "Sandbox":
        """Return the handle of an existing sandbox, with its info loaded."""
        info = self._request("GET", f"/v1/sandboxes/{_segment(sandbox_id)}").json()
        return Sandbox(self, info["id"], SandboxInfo.from_dict(info))

    # --- secrets --------------------------------------------------------------------------

    def put_secret(self, name: str, value: str) -> Secret:
        """Create a secret or replace its value. Needs the scope secrets:manage.

        The API never returns the value. A new value reaches the next command of
        the sandboxes that already name the secret.
        """
        return Secret.from_dict(self._request("PUT", f"/v1/secrets/{_segment(name)}", json_body={"value": value}).json())

    def list_secrets(self) -> list[Secret]:
        """List the secrets of the tenant ordered by name, without their values."""
        r = self._request("GET", "/v1/secrets").json()
        return [Secret.from_dict(s) for s in r.get("secrets") or []]

    def delete_secret(self, name: str) -> None:
        """Delete a secret. Commands of a sandbox that names it then raise ConflictError."""
        self._request("DELETE", f"/v1/secrets/{_segment(name)}")

    # --- snapshots ------------------------------------------------------------------------

    def create_snapshot(self, sandbox_id: str, name: str) -> Snapshot:
        """Save the state of a running sandbox under a name. The sandbox keeps running.

        Start sandboxes from it with create_sandbox(snapshot=snapshot.id).
        """
        path = f"/v1/sandboxes/{_segment(sandbox_id)}/snapshots"
        return Snapshot.from_dict(self._request("POST", path, json_body={"name": name}).json())

    def list_snapshots(self) -> list[Snapshot]:
        """List the snapshots of the tenant, oldest first."""
        r = self._request("GET", "/v1/snapshots").json()
        return [Snapshot.from_dict(s) for s in r.get("snapshots") or []]

    def get_snapshot(self, snapshot_id: str) -> Snapshot:
        """Return one snapshot."""
        return Snapshot.from_dict(self._request("GET", f"/v1/snapshots/{_segment(snapshot_id)}").json())

    def delete_snapshot(self, snapshot_id: str) -> None:
        """Delete a snapshot. Sandboxes already started from it keep running."""
        self._request("DELETE", f"/v1/snapshots/{_segment(snapshot_id)}")

    # --- volumes --------------------------------------------------------------------------

    def create_volume(self, sandbox_id: str, name: str, size_gb: int) -> Volume:
        """Create a durable volume mounted at /workspace/<name> of the sandbox.

        The name of an existing detached volume reattaches it.
        """
        body = {"sandbox_id": sandbox_id, "name": name, "size_gb": size_gb}
        return Volume.from_dict(self._request("POST", "/v1/volumes", json_body=body).json())

    def list_volumes(self, sandbox_id: str) -> list[Volume]:
        """List the volumes attached to a sandbox."""
        r = self._request("GET", "/v1/volumes", query={"sandbox_id": sandbox_id}).json()
        return [Volume.from_dict(v) for v in r.get("volumes") or []]

    def delete_volume(self, volume_id: str) -> None:
        """Delete a volume and its data."""
        self._request("DELETE", f"/v1/volumes/{_segment(volume_id)}")

    # --- API keys -------------------------------------------------------------------------

    def list_api_keys(self) -> list[ApiKey]:
        """List the tenant's API keys, revoked ones included. Needs "api_keys:manage"."""
        r = self._request("GET", "/v1/api-keys").json()
        return [ApiKey.from_dict(k) for k in r.get("api_keys") or []]

    def create_api_key(self, name: str, scopes: Optional[Sequence[str]] = None) -> ApiKey:
        """Create an API key. The secret is in the key field of this answer only.

        scopes defaults, on the server, to ["sandbox:execute"].
        """
        body: dict = {"name": name}
        if scopes is not None:
            body["scopes"] = list(scopes)
        return ApiKey.from_dict(self._request("POST", "/v1/api-keys", json_body=body).json())

    def revoke_api_key(self, key_id: str) -> None:
        """Revoke an API key. It is refused from the next request on."""
        self._request("DELETE", f"/v1/api-keys/{_segment(key_id)}")

    # --- templates ------------------------------------------------------------------------

    def list_templates(self) -> list[Template]:
        """List the built-in templates, the tenant's own and the public ones."""
        r = self._request("GET", "/v1/templates").json()
        return [Template.from_dict(t) for t in r.get("templates") or []]

    def register_template(self, name: str, image: str, *, version: str = "", public: bool = False) -> Template:
        """Register a template from a digest-pinned image (name@sha256:...)."""
        body: dict = {"name": name, "image": image}
        if version:
            body["version"] = version
        if public:
            body["public"] = True
        return Template.from_dict(self._request("POST", "/v1/templates", json_body=body).json())

    def get_template(self, template_id: str) -> Template:
        """Return a registered template."""
        return Template.from_dict(self._request("GET", f"/v1/templates/{_segment(template_id)}").json())

    # --- platform -------------------------------------------------------------------------

    def get_capabilities(self) -> Capabilities:
        """Return what the runtime driver of the deployment supports."""
        return Capabilities.from_dict(self._request("GET", "/v1/capabilities").json())

    def health(self) -> dict:
        """Check liveness. Returns the body of the answer; sends no API key."""
        return self._request("GET", "/v1/healthz", auth=False).json()

    def ready(self) -> dict:
        """Check readiness. Raises ServerError when a dependency is unreachable."""
        return self._request("GET", "/v1/readyz", auth=False).json()

    def prometheus_metrics(self) -> str:
        """Return the Prometheus text exposition of the deployment; sends no API key."""
        return self._request("GET", "/metrics", auth=False).body.decode()

    # --- signed file URLs -----------------------------------------------------------------

    def _signed_url(self, url_or_token: str) -> str:
        if "://" in url_or_token:
            return url_or_token
        return self._base + _SIGNED_FILES_PATH + _segment(url_or_token)

    def download_signed_file(self, url_or_token: str) -> bytes:
        """Download a file with a URL from Sandbox.create_file_url, or its token.

        The request carries no API key: the URL is the credential.
        """
        return self._request("GET", "", url=self._signed_url(url_or_token), auth=False).body

    def upload_signed_file(self, url_or_token: str, data: bytes) -> None:
        """Upload a file with a URL from Sandbox.create_file_url, or its token.

        The request carries no API key: the URL is the credential.
        """
        self._request("PUT", "", url=self._signed_url(url_or_token), data=data, auth=False)


class Sandbox:
    """One sandbox. info is what the API last said about it, when known."""

    def __init__(self, client: LoopSandbox, sandbox_id: str, info: Optional[SandboxInfo] = None):
        self._c = client
        self.id = sandbox_id
        self.info = info
        self._default_session: Optional["Session"] = None

    def _path(self, suffix: str = "") -> str:
        return f"/v1/sandboxes/{_segment(self.id)}{suffix}"

    def _new(self, info: Mapping[str, Any]) -> "Sandbox":
        return Sandbox(self._c, info["id"], SandboxInfo.from_dict(info))

    # --- lifecycle ------------------------------------------------------------------------

    def refresh(self) -> SandboxInfo:
        """Read the sandbox again and return its info."""
        self.info = SandboxInfo.from_dict(self._c._request("GET", self._path()).json())
        return self.info

    def set_timeout(self, ttl_seconds: int) -> SandboxInfo:
        """Make the sandbox expire ttl_seconds from now, shortening or extending it."""
        r = self._c._request("POST", self._path("/timeout"), json_body={"ttl_seconds": ttl_seconds}).json()
        self.info = SandboxInfo.from_dict(r)
        return self.info

    def fork(self) -> "Sandbox":
        """Clone this running sandbox into a new isolated one."""
        return self._new(self._c._request("POST", self._path("/fork")).json())

    def fork_many(self, count: int) -> list["Sandbox"]:
        """Create count independent forks. The batch is all or nothing."""
        r = self._c._request("POST", self._path("/fork"), json_body={"count": count}).json()
        return [self._new(s) for s in r["sandboxes"]]

    def pause(self) -> None:
        """Snapshot the sandbox to durable storage and release its compute."""
        self._c._request("POST", self._path("/pause"))

    def resume(self) -> SandboxInfo:
        """Resume the paused sandbox."""
        self.info = SandboxInfo.from_dict(self._c._request("POST", self._path("/resume")).json())
        return self.info

    def destroy(self) -> None:
        self._c._request("DELETE", self._path())

    def metrics(self) -> SandboxMetrics:
        """Read the CPU and memory use of the sandbox."""
        return SandboxMetrics.from_dict(self._c._request("GET", self._path("/metrics")).json())

    def logs(self, after: int = 0, wait_ms: int = 0) -> SandboxLogs:
        """Read what the background processes wrote after the cursor.

        Send the next of one answer as after in the following call. With no output
        after the cursor, the call waits up to wait_ms (at most 30000) for some.
        """
        r = self._c._request("GET", self._path("/logs"), query=_cursor_query(after, wait_ms)).json()
        return SandboxLogs.from_dict(r)

    # --- executions -----------------------------------------------------------------------

    @staticmethod
    def _exec_body(cmd: Sequence[str], workdir: str, env: Optional[Mapping[str, str]], timeout_ms: int) -> dict:
        body: dict = {"cmd": list(cmd)}
        if workdir:
            body["workdir"] = workdir
        if env:
            body["env"] = dict(env)
        if timeout_ms:
            body["timeout_ms"] = timeout_ms
        return body

    def run(
        self,
        cmd: list[str],
        workdir: str = "",
        *,
        env: Optional[Mapping[str, str]] = None,
        timeout_ms: int = 0,
    ) -> ExecResult:
        """Run a command and wait for it. A non-zero exit code is a normal result."""
        body = self._exec_body(cmd, workdir, env, timeout_ms)
        r = self._c._request("POST", self._path("/executions"), json_body=body).json()
        return ExecResult(r["exit_code"], r["stdout"], r["stderr"])

    def run_stream(
        self,
        cmd: list[str],
        workdir: str = "",
        *,
        env: Optional[Mapping[str, str]] = None,
        timeout_ms: int = 0,
    ) -> Iterator[StreamEvent]:
        """Run a command and yield its output while it runs.

        Yields "stdout" and "stderr" events, then one "done" event with the exit
        code. Raises StreamError when the command fails after output started.
        """
        return self._c._stream(self._path("/executions"), self._exec_body(cmd, workdir, env, timeout_ms))

    # --- background processes -------------------------------------------------------------

    def _process_path(self, pid: int, suffix: str = "") -> str:
        return self._path(f"/processes/{_segment(pid)}{suffix}")

    def start_process(
        self,
        cmd: list[str],
        *,
        env: Optional[Mapping[str, str]] = None,
        workdir: str = "",
    ) -> Process:
        """Start a command in the background and return at once."""
        body = self._exec_body(cmd, workdir, env, 0)
        return Process.from_dict(self._c._request("POST", self._path("/processes"), json_body=body).json())

    def list_processes(self) -> list[Process]:
        """List the background processes, oldest first, recent ended ones included."""
        r = self._c._request("GET", self._path("/processes")).json()
        return [Process.from_dict(p) for p in r.get("processes") or []]

    def get_process(self, pid: int) -> Process:
        return Process.from_dict(self._c._request("GET", self._process_path(pid)).json())

    def process_output(self, pid: int, after: int = 0, wait_ms: int = 0) -> ProcessOutput:
        """Read what a process wrote after the cursor, and its state.

        Send the next of one answer as after in the following call; after=0 reads
        from the start. With no output after the cursor, the call waits up to
        wait_ms (at most 30000) for some, or for the process to end.
        """
        r = self._c._request("GET", self._process_path(pid, "/output"), query=_cursor_query(after, wait_ms)).json()
        return ProcessOutput.from_dict(r)

    def write_process_stdin(self, pid: int, data: bytes) -> None:
        """Write bytes to the standard input of a process, up to 1 MiB a call."""
        self._c._request("POST", self._process_path(pid, "/stdin"), data=data)

    def close_process_stdin(self, pid: int) -> None:
        """Close the standard input of a process, so it reads the end of its input."""
        self._c._request("DELETE", self._process_path(pid, "/stdin"))

    def signal_process(self, pid: int, signal: str) -> None:
        """Send a signal, such as "TERM" or "KILL", to a process and its children."""
        self._c._request("POST", self._process_path(pid, "/signal"), json_body={"signal": signal})

    # --- files ----------------------------------------------------------------------------

    def write_file(self, path: str, data: bytes) -> None:
        self._c._request("PUT", self._path(f"/files/{_file_path(path)}"), data=data)

    def read_file(self, path: str) -> bytes:
        return self._c._request("GET", self._path(f"/files/{_file_path(path)}")).body

    def delete_file(self, path: str) -> None:
        self._c._request("DELETE", self._path(f"/files/{_file_path(path)}"))

    def list_files(self, path: str = "") -> list[FileEntry]:
        """List the immediate children of a directory. Defaults to /workspace."""
        query = {"path": path} if path else None
        r = self._c._request("GET", self._path("/files"), query=query).json()
        return [FileEntry.from_dict(f) for f in r.get("files") or []]

    def file_info(self, path: str) -> FileInfo:
        """Describe a file or directory. A symbolic link is described as the link."""
        return FileInfo.from_dict(self._c._request("GET", self._path(f"/file-info/{_file_path(path)}")).json())

    def make_directory(self, path: str) -> None:
        """Create a directory and any missing parent. An existing one is not an error."""
        self._c._request("PUT", self._path(f"/directories/{_file_path(path)}"))

    def move_file(self, from_path: str, to_path: str, *, overwrite: bool = False) -> None:
        """Move or rename a file or directory, creating the parents of to_path.

        An existing to_path raises ConflictError (code "already_exists") unless
        overwrite is true.
        """
        body: dict = {"from": from_path, "to": to_path}
        if overwrite:
            body["overwrite"] = True
        self._c._request("POST", self._path("/file-moves"), json_body=body)

    def create_file_url(self, path: str, operation: str, *, expires_in_seconds: int = 0) -> SignedFileUrl:
        """Create a signed URL to "upload" or "download" one file without an API key.

        expires_in_seconds defaults, on the server, to 300 and is at most 3600.
        """
        body: dict = {"path": path, "operation": operation}
        if expires_in_seconds:
            body["expires_in_seconds"] = expires_in_seconds
        return SignedFileUrl.from_dict(self._c._request("POST", self._path("/file-urls"), json_body=body).json())

    def artifact_url(self, path: str) -> ArtifactUrl:
        """Copy a file to object storage and return a time-limited download URL."""
        return ArtifactUrl.from_dict(self._c._request("GET", self._path(f"/artifact-url/{_file_path(path)}")).json())

    # --- directory watches ----------------------------------------------------------------

    def start_watch(self, path: str, *, recursive: bool = False) -> Watch:
        """Start recording the changes to a directory. Read them with watch_events."""
        body: dict = {"path": path}
        if recursive:
            body["recursive"] = True
        return Watch.from_dict(self._c._request("POST", self._path("/watches"), json_body=body).json())

    def watch_events(self, watch_id: str, after: int = 0, wait_ms: int = 0) -> FileEvents:
        """Read the events of a watch after the cursor.

        Send the next of one answer as after in the following call. With no event
        after the cursor, the call waits up to wait_ms (at most 30000) for one.
        """
        path = self._path(f"/watches/{_segment(watch_id)}/events")
        return FileEvents.from_dict(self._c._request("GET", path, query=_cursor_query(after, wait_ms)).json())

    def stop_watch(self, watch_id: str) -> None:
        self._c._request("DELETE", self._path(f"/watches/{_segment(watch_id)}"))

    # --- code sessions --------------------------------------------------------------------

    def start_session(self) -> "Session":
        r = self._c._request("POST", self._path("/sessions"), json_body={}).json()
        return Session(self._c, self.id, r["session_id"])

    def run_code(self, code: str) -> CellResult:
        """Run a cell in a default session created on first use, so that state is
        shared across run_code() calls."""
        if self._default_session is None:
            self._default_session = self.start_session()
        return self._default_session.execute(code)

    # --- browser --------------------------------------------------------------------------

    def browser_navigate(self, url: str) -> None:
        """Navigate the sandbox browser to a URL the egress policy allows."""
        self._c._request("POST", self._path("/browser/navigate"), json_body={"url": url})

    def browser_screenshot(self) -> bytes:
        """Capture the current page as PNG bytes."""
        return self._c._request("GET", self._path("/browser/screenshot")).body

    def browser_dom(self) -> str:
        """Return the HTML of the current page."""
        return self._c._request("GET", self._path("/browser/dom")).json()["dom"]

    def browser_input(
        self,
        action: str,
        *,
        x: Optional[float] = None,
        y: Optional[float] = None,
        dx: Optional[float] = None,
        dy: Optional[float] = None,
        text: Optional[str] = None,
    ) -> None:
        """Send a "click" (x, y), a "type" (text) or a "scroll" (x, y, dx, dy) to the page."""
        body: dict = {"action": action}
        for name, value in (("x", x), ("y", y), ("dx", dx), ("dy", dy), ("text", text)):
            if value is not None:
                body[name] = value
        self._c._request("POST", self._path("/browser/input"), json_body=body)


@dataclass
class SandboxPage:
    """One page of list_sandboxes. next_cursor is empty on the last page."""

    sandboxes: list[Sandbox] = field(default_factory=list)
    next_cursor: str = ""


class Session:
    def __init__(self, client: LoopSandbox, sandbox_id: str, session_id: str):
        self._c = client
        self._sid = sandbox_id
        self.id = session_id

    def _path(self, suffix: str = "") -> str:
        return f"/v1/sandboxes/{_segment(self._sid)}/sessions/{_segment(self.id)}{suffix}"

    def execute(self, code: str) -> CellResult:
        r = self._c._request("POST", self._path("/execute"), json_body={"code": code}).json()
        return CellResult(r["exit_code"], r["stdout"], r["stderr"])

    def execute_stream(self, code: str) -> Iterator[StreamEvent]:
        """Run a cell and yield its output while it runs.

        Yields "stdout" and "stderr" events, then one "done" event with the exit
        code and the cell's stderr. Raises StreamError when the cell cannot run.
        """
        return self._c._stream(self._path("/execute"), {"code": code})

    def close(self) -> None:
        self._c._request("DELETE", self._path())
