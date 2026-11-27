"""Agent-framework adapters (M20 agent-native DX): expose a LoopSandbox sandbox as tools for
popular agent frameworks so an agent can run code / commands / file ops in an isolated sandbox
with a one-liner. Two frameworks are supported: OpenAI function/tool-calling and LangChain.

The adapters are thin and dependency-light: the OpenAI adapter is pure-stdlib (returns the tool
JSON schema + a dispatcher); the LangChain adapter imports langchain lazily so this module never
forces langchain as a dependency.
"""
from __future__ import annotations

from typing import Any, Callable

from . import Sandbox


# --- OpenAI tool-calling adapter -------------------------------------------------------------

def openai_tools() -> list[dict]:
    """Return the OpenAI `tools` schema for the sandbox verbs (run_code, run, write_file,
    read_file). Pass these to the Chat Completions / Responses API `tools=` parameter."""
    return [
        {
            "type": "function",
            "function": {
                "name": "sandbox_run_code",
                "description": "Run Python code in the sandbox; state is shared across calls.",
                "parameters": {
                    "type": "object",
                    "properties": {"code": {"type": "string"}},
                    "required": ["code"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "sandbox_run",
                "description": "Run a shell-free command (argv) in the sandbox.",
                "parameters": {
                    "type": "object",
                    "properties": {"cmd": {"type": "array", "items": {"type": "string"}}},
                    "required": ["cmd"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "sandbox_write_file",
                "description": "Write a text file under /workspace in the sandbox.",
                "parameters": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                    "required": ["path", "content"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "sandbox_read_file",
                "description": "Read a text file under /workspace in the sandbox.",
                "parameters": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"],
                },
            },
        },
    ]


def openai_dispatch(sandbox: Sandbox, name: str, args: dict[str, Any]) -> str:
    """Execute one OpenAI tool call against `sandbox` and return a string result to feed back to
    the model as the tool message. Raises ValueError for an unknown tool name (fail-fast)."""
    if name == "sandbox_run_code":
        r = sandbox.run_code(args["code"])
        return r.stdout if r.exit_code == 0 else f"error (exit {r.exit_code}): {r.stderr}"
    if name == "sandbox_run":
        r = sandbox.run(list(args["cmd"]))
        return r.stdout if r.exit_code == 0 else f"error (exit {r.exit_code}): {r.stderr}"
    if name == "sandbox_write_file":
        sandbox.write_file(args["path"], args["content"].encode())
        return "ok"
    if name == "sandbox_read_file":
        return sandbox.read_file(args["path"]).decode(errors="replace")
    raise ValueError(f"unknown sandbox tool: {name}")


# --- LangChain adapter ------------------------------------------------------------------------

def langchain_tools(sandbox: Sandbox) -> list:
    """Return LangChain StructuredTools bound to `sandbox`. langchain is imported lazily so this
    module has no hard langchain dependency; a clear ImportError is raised if it is missing."""
    try:
        from langchain_core.tools import StructuredTool
    except ImportError as e:  # pragma: no cover - exercised only without langchain installed
        raise ImportError(
            "langchain_tools requires langchain-core; install it with `pip install langchain-core`"
        ) from e

    def _run_code(code: str) -> str:
        return openai_dispatch(sandbox, "sandbox_run_code", {"code": code})

    def _run(cmd: list[str]) -> str:
        return openai_dispatch(sandbox, "sandbox_run", {"cmd": cmd})

    return [
        StructuredTool.from_function(_run_code, name="sandbox_run_code",
                                     description="Run Python code in an isolated sandbox (state shared)."),
        StructuredTool.from_function(_run, name="sandbox_run",
                                     description="Run a shell-free command (argv) in an isolated sandbox."),
    ]


# A generic callable-map adapter for any framework that takes {name: fn}.
def tool_functions(sandbox: Sandbox) -> dict[str, Callable[..., str]]:
    """Return a {tool_name: callable} map for frameworks that accept plain callables."""
    return {
        "sandbox_run_code": lambda code: openai_dispatch(sandbox, "sandbox_run_code", {"code": code}),
        "sandbox_run": lambda cmd: openai_dispatch(sandbox, "sandbox_run", {"cmd": cmd}),
        "sandbox_write_file": lambda path, content: openai_dispatch(sandbox, "sandbox_write_file", {"path": path, "content": content}),
        "sandbox_read_file": lambda path: openai_dispatch(sandbox, "sandbox_read_file", {"path": path}),
    }
