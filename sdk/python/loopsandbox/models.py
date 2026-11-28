"""Typed results of the LoopSandbox API.

Timestamps stay as the RFC 3339 strings the API sends. Optional fields the API
omits are empty strings, empty collections or None.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping, Optional

__all__ = [
    "ExecResult",
    "CellResult",
    "StreamEvent",
    "SandboxInfo",
    "FileEntry",
    "FileInfo",
    "SignedFileUrl",
    "ArtifactUrl",
    "Process",
    "ProcessOutput",
    "LogEntry",
    "SandboxLogs",
    "SandboxMetrics",
    "Watch",
    "FileEvent",
    "FileEvents",
    "Volume",
    "ApiKey",
    "Capability",
    "Capabilities",
    "Template",
]


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass
class CellResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass
class StreamEvent:
    """One event of a streamed execution.

    type is "stdout" or "stderr" for an output chunk, whose text is in data, and
    "done" for the last event, which carries exit_code. The "done" of a code cell
    also carries the cell's stderr.
    """

    type: str
    data: str = ""
    exit_code: Optional[int] = None
    stderr: str = ""


@dataclass
class NetworkPolicy:
    """The outbound rules of a sandbox.

    With no rules a sandbox reaches the public internet. An entry of allow_out
    wins over deny_out, so the usual way to restrict a sandbox is
    deny_out=["0.0.0.0/0"] plus the hosts it needs in allow_out. Private and
    cloud metadata addresses are always refused.
    """

    allow_out: list[str] = field(default_factory=list)  # addresses, CIDR ranges, host names, "*.name", "*"
    deny_out: list[str] = field(default_factory=list)  # addresses and CIDR ranges only

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "NetworkPolicy":
        return cls(allow_out=list(d.get("allow_out") or []), deny_out=list(d.get("deny_out") or []))

    def to_dict(self) -> dict[str, list[str]]:
        return {"allow_out": list(self.allow_out), "deny_out": list(self.deny_out)}


@dataclass
class NetworkDenial:
    """One outbound connection the rules of a sandbox refused."""

    time: str
    destination: str  # address and port the sandbox tried to reach
    reason: str
    host: str = ""  # the name it asked for, when the protocol carried one

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "NetworkDenial":
        return cls(time=d["time"], destination=d["destination"], reason=d.get("reason", ""), host=d.get("host", ""))


@dataclass
class SandboxInfo:
    id: str
    tenant_id: str = ""
    state: str = ""
    image: str = ""
    gpu: str = ""
    created_at: str = ""
    expires_at: str = ""
    metadata: dict[str, str] = field(default_factory=dict)
    env: dict[str, str] = field(default_factory=dict)
    auto_pause_seconds: int = 0  # 0 when the sandbox does not pause itself
    secrets: list[str] = field(default_factory=list)  # names only
    network: Optional[NetworkPolicy] = None  # None when the sandbox has the default policy

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "SandboxInfo":
        return cls(
            id=d["id"],
            tenant_id=d.get("tenant_id", ""),
            state=d.get("state", ""),
            image=d.get("image", ""),
            gpu=d.get("gpu", ""),
            created_at=d.get("created_at", ""),
            expires_at=d.get("expires_at", ""),
            metadata=dict(d.get("metadata") or {}),
            env=dict(d.get("env") or {}),
            auto_pause_seconds=d.get("auto_pause_seconds", 0),
            secrets=list(d.get("secrets") or []),
            network=NetworkPolicy.from_dict(d["network"]) if d.get("network") else None,
        )


@dataclass
class FileEntry:
    name: str
    size: int
    is_dir: bool

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "FileEntry":
        return cls(d["name"], d["size"], d["is_dir"])


@dataclass
class FileInfo:
    name: str
    path: str
    size: int
    is_dir: bool
    mode: str  # permission bits in octal, such as "0644"
    modified_at: str

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "FileInfo":
        return cls(d["name"], d["path"], d["size"], d["is_dir"], d["mode"], d["modified_at"])


@dataclass
class SignedFileUrl:
    url: str
    method: str  # "GET" for a download, "PUT" for an upload
    expires_at: str

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "SignedFileUrl":
        return cls(d["url"], d["method"], d["expires_at"])


@dataclass
class ArtifactUrl:
    url: str
    expires_in_seconds: int

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "ArtifactUrl":
        return cls(d["url"], d["expires_in_seconds"])


@dataclass
class Process:
    pid: int
    cmd: list[str]
    started_at: str
    running: bool
    exit_code: Optional[int]  # None while the process runs

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Process":
        return cls(d["pid"], list(d.get("cmd") or []), d.get("started_at", ""), d["running"], d.get("exit_code"))


@dataclass
class ProcessOutput:
    stdout: str
    stderr: str
    next: int  # cursor to send as "after" in the following read
    truncated: bool
    process: Process

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "ProcessOutput":
        return cls(d["stdout"], d["stderr"], d["next"], d["truncated"], Process.from_dict(d["process"]))


@dataclass
class LogEntry:
    pid: int
    stream: str  # "stdout" or "stderr"
    data: str
    at: str

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "LogEntry":
        return cls(d["pid"], d["stream"], d["data"], d["at"])


@dataclass
class SandboxLogs:
    entries: list[LogEntry]
    next: int  # cursor to send as "after" in the following read
    truncated: bool

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "SandboxLogs":
        return cls([LogEntry.from_dict(e) for e in d.get("entries") or []], d["next"], d["truncated"])


@dataclass
class SandboxMetrics:
    cpu_usage_ns: int
    memory_used_bytes: int
    memory_limit_bytes: int
    at: str
    disk_used_bytes: Optional[int] = None  # None when the driver cannot read it, as for a paused sandbox
    disk_limit_bytes: Optional[int] = None  # size of the workspace; None with disk_used_bytes

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "SandboxMetrics":
        limit = d.get("disk_limit_bytes")
        return cls(
            d["cpu_usage_ns"],
            d["memory_used_bytes"],
            d["memory_limit_bytes"],
            d["at"],
            d.get("disk_used_bytes", 0) if limit else None,
            limit or None,
        )


@dataclass
class Watch:
    watch_id: str
    path: str
    recursive: bool

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Watch":
        return cls(d["watch_id"], d["path"], d["recursive"])


@dataclass
class FileEvent:
    type: str  # create, write, remove, rename, chmod or overflow
    is_dir: bool
    path: str = ""  # empty in an "overflow" event

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "FileEvent":
        return cls(d["type"], d["is_dir"], d.get("path", ""))


@dataclass
class FileEvents:
    events: list[FileEvent]
    next: int  # cursor to send as "after" in the following read
    truncated: bool
    closed: bool

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "FileEvents":
        return cls([FileEvent.from_dict(e) for e in d.get("events") or []], d["next"], d["truncated"], d["closed"])


@dataclass
class Volume:
    id: str
    name: str
    size_gb: int
    created_at: str
    sandbox_id: str = ""  # empty while the volume is detached

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Volume":
        return cls(d["id"], d["name"], d["size_gb"], d.get("created_at", ""), d.get("sandbox_id", ""))


@dataclass
class Secret:
    """A secret of the tenant. The value is write-only: the API never returns it."""

    name: str
    created_at: str = ""
    updated_at: str = ""

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Secret":
        return cls(d["name"], d.get("created_at", ""), d.get("updated_at", ""))


@dataclass
class Snapshot:
    """The saved memory and disk state of a sandbox, from which new sandboxes start."""

    id: str
    name: str
    sandbox_id: str  # the sandbox it was taken from, which may no longer exist
    size_bytes: int
    created_at: str
    image: str = ""

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Snapshot":
        return cls(d["id"], d["name"], d["sandbox_id"], d.get("size_bytes", 0), d.get("created_at", ""), d.get("image", ""))


@dataclass
class ApiKey:
    id: str
    name: str
    prefix: str
    scopes: list[str]
    created_at: str
    revoked_at: str = ""
    # The secret. Present only in the answer of create_api_key, and kept out of repr.
    key: str = field(default="", repr=False)

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "ApiKey":
        return cls(
            d["id"], d["name"], d.get("prefix", ""), list(d.get("scopes") or []),
            d.get("created_at", ""), d.get("revoked_at", ""), d.get("key", ""),
        )


@dataclass
class Capability:
    name: str
    supported: bool


@dataclass
class Capabilities:
    driver: str
    capabilities: list[Capability]

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Capabilities":
        return cls(d["driver"], [Capability(c["name"], c["supported"]) for c in d.get("capabilities") or []])

    def supports(self, name: str) -> bool:
        """Whether the deployment supports the capability. An unknown name is False."""
        return any(c.name == name and c.supported for c in self.capabilities)


@dataclass
class Template:
    """A template. A built-in one has name, image, description and builtin=True."""

    name: str
    image: str
    id: str = ""
    version: str = ""
    digest: str = ""
    public: bool = False
    scan_status: str = ""
    description: str = ""
    builtin: bool = False

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> "Template":
        return cls(
            name=d["name"],
            image=d["image"],
            id=d.get("id", ""),
            version=d.get("version", ""),
            digest=d.get("digest", ""),
            public=bool(d.get("public", False)),
            scan_status=d.get("scan_status", ""),
            description=d.get("description", ""),
            builtin=bool(d.get("builtin", False)),
        )
