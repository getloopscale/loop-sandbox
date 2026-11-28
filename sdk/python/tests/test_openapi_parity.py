"""The SDK covers every operation of api/openapi.yaml.

Adding an operation to the API fails this test until the SDK gains a method for it
and COVERAGE names that method. Removing one fails it until its entry is removed.
"""
from pathlib import Path

import pytest
import yaml

import loopsandbox

OPENAPI_PATH = Path(__file__).resolve().parents[3] / "api" / "openapi.yaml"
HTTP_METHODS = {"get", "put", "post", "delete", "patch", "head", "options", "trace"}

# operationId -> "Class.method" in the loopsandbox package.
COVERAGE = {
    "listSandboxes": "LoopSandbox.list_sandboxes",
    "createSandbox": "LoopSandbox.create_sandbox",
    "getSandbox": "LoopSandbox.get_sandbox",
    "destroySandbox": "SandboxHandle.destroy",
    "setSandboxTimeout": "SandboxHandle.set_timeout",
    "updateSandboxNetwork": "SandboxHandle.update_network",
    "listNetworkDenials": "SandboxHandle.network_denials",
    "forkSandbox": "SandboxHandle.fork",
    "pauseSandbox": "SandboxHandle.pause",
    "resumeSandbox": "SandboxHandle.resume",
    "createExecution": "SandboxHandle.run",
    "listFiles": "SandboxHandle.list_files",
    "putFile": "SandboxHandle.write_file",
    "getFile": "SandboxHandle.read_file",
    "deleteFile": "SandboxHandle.delete_file",
    "getFileInfo": "SandboxHandle.file_info",
    "makeDirectory": "SandboxHandle.make_directory",
    "moveFile": "SandboxHandle.move_file",
    "createFileUrl": "SandboxHandle.create_file_url",
    "downloadSignedFile": "LoopSandbox.download_signed_file",
    "uploadSignedFile": "LoopSandbox.upload_signed_file",
    "getArtifactUrl": "SandboxHandle.artifact_url",
    "getSandboxLogs": "SandboxHandle.logs",
    "getSandboxMetrics": "SandboxHandle.metrics",
    "startWatch": "SandboxHandle.start_watch",
    "getWatchEvents": "SandboxHandle.watch_events",
    "stopWatch": "SandboxHandle.stop_watch",
    "startProcess": "SandboxHandle.start_process",
    "listProcesses": "SandboxHandle.list_processes",
    "getProcess": "SandboxHandle.get_process",
    "getProcessOutput": "SandboxHandle.process_output",
    "writeProcessStdin": "SandboxHandle.write_process_stdin",
    "closeProcessStdin": "SandboxHandle.close_process_stdin",
    "signalProcess": "SandboxHandle.signal_process",
    "startSession": "SandboxHandle.start_session",
    "runCell": "Session.execute",
    "closeSession": "Session.close",
    "browserNavigate": "SandboxHandle.browser_navigate",
    "browserScreenshot": "SandboxHandle.browser_screenshot",
    "browserDom": "SandboxHandle.browser_dom",
    "browserInput": "SandboxHandle.browser_input",
    "putSecret": "LoopSandbox.put_secret",
    "listSecrets": "LoopSandbox.list_secrets",
    "deleteSecret": "LoopSandbox.delete_secret",
    "createSnapshot": "LoopSandbox.create_snapshot",
    "listSnapshots": "LoopSandbox.list_snapshots",
    "getSnapshot": "LoopSandbox.get_snapshot",
    "deleteSnapshot": "LoopSandbox.delete_snapshot",
    "createVolume": "LoopSandbox.create_volume",
    "listVolumes": "LoopSandbox.list_volumes",
    "deleteVolume": "LoopSandbox.delete_volume",
    "listApiKeys": "LoopSandbox.list_api_keys",
    "createApiKey": "LoopSandbox.create_api_key",
    "revokeApiKey": "LoopSandbox.revoke_api_key",
    "getCapabilities": "LoopSandbox.get_capabilities",
    "listTemplates": "LoopSandbox.list_templates",
    "registerTemplate": "LoopSandbox.register_template",
    "getTemplate": "LoopSandbox.get_template",
    "healthz": "LoopSandbox.health",
    "readyz": "LoopSandbox.ready",
    "metrics": "LoopSandbox.prometheus_metrics",
}

# operationId -> why the SDK has no typed method for it. Keep this as small as possible.
EXCLUSIONS = {
    "openPty": (
        "The route upgrades to a WebSocket. The SDK has no runtime dependency and the "
        "standard library has no WebSocket client, so there is nothing typed to wrap."
    ),
}


def spec_operation_ids():
    spec = yaml.safe_load(OPENAPI_PATH.read_text(encoding="utf-8"))
    ids = []
    for path, item in spec["paths"].items():
        for method, operation in item.items():
            if method not in HTTP_METHODS:
                continue
            assert "operationId" in operation, f"{method.upper()} {path} has no operationId"
            ids.append(operation["operationId"])
    return ids


def resolve(attribute_path):
    """Return the object an attribute path such as "SandboxHandle.fork" names in the package."""
    target = loopsandbox
    for name in attribute_path.split("."):
        target = getattr(target, name)
    return target


def test_spec_declares_unique_operation_ids():
    ids = spec_operation_ids()

    assert ids, "no operation found in the OpenAPI document"
    assert sorted(set(ids)) == sorted(ids)


def test_every_api_operation_is_covered_or_excluded():
    declared = set(spec_operation_ids())

    missing = sorted(declared - set(COVERAGE) - set(EXCLUSIONS))

    assert missing == [], f"API operations with no SDK method and no exclusion: {missing}"


def test_coverage_table_names_only_operations_the_api_has():
    declared = set(spec_operation_ids())

    stale = sorted(set(COVERAGE) - declared)

    assert stale == [], f"COVERAGE names operations the API no longer has: {stale}"


def test_exclusions_table_names_only_operations_the_api_has():
    declared = set(spec_operation_ids())

    stale = sorted(set(EXCLUSIONS) - declared)

    assert stale == [], f"EXCLUSIONS names operations the API no longer has: {stale}"


def test_no_operation_is_both_covered_and_excluded():
    assert sorted(set(COVERAGE) & set(EXCLUSIONS)) == []


@pytest.mark.parametrize("operation_id, attribute_path", sorted(COVERAGE.items()))
def test_covered_operation_names_a_callable_public_method(operation_id, attribute_path):
    owner_name, _, method_name = attribute_path.partition(".")

    method = resolve(attribute_path)

    assert owner_name in loopsandbox.__all__, f"{owner_name} is not a public name of loopsandbox"
    assert method_name and not method_name.startswith("_"), f"{attribute_path} is not a public method"
    assert callable(method), f"{attribute_path} is not callable"


@pytest.mark.parametrize("operation_id, reason", sorted(EXCLUSIONS.items()))
def test_exclusion_has_a_written_reason(operation_id, reason):
    assert len(reason.strip()) >= 20, f"{operation_id} is excluded without a real reason"


def test_each_sdk_method_covers_one_operation():
    targets = list(COVERAGE.values())

    assert sorted(set(targets)) == sorted(targets)
