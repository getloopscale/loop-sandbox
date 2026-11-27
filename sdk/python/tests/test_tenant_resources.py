"""Volumes, API keys, templates, capabilities and the platform probes."""
import pytest

from loopsandbox import ApiKey, Capabilities, Capability, ConflictError, Secret, ServerError, Snapshot, Template, Volume

VOLUME = {"id": "vol_1", "name": "data", "size_gb": 5, "sandbox_id": "sbx_1", "created_at": "2026-11-20T10:00:00Z"}
API_KEY = {"id": "key_1", "name": "ci", "prefix": "lsb_ab12", "scopes": ["sandbox:execute"],
           "created_at": "2026-11-20T10:00:00Z"}
TEMPLATE = {"id": "tpl_1", "name": "ml", "image": "ml@sha256:abc", "version": "1", "digest": "sha256:abc",
            "public": True, "scan_status": "passed"}


def test_create_volume_sends_sandbox_name_and_size(api):
    api.answer(201, VOLUME)

    volume = api.client.create_volume("sbx_1", "data", 5)

    assert (api.last.method, api.last.path) == ("POST", "/v1/volumes")
    assert api.last.json == {"sandbox_id": "sbx_1", "name": "data", "size_gb": 5}
    assert volume == Volume("vol_1", "data", 5, "2026-11-20T10:00:00Z", "sbx_1")


def test_list_volumes_filters_by_sandbox(api):
    api.answer(200, {"volumes": [VOLUME]})

    volumes = api.client.list_volumes("sbx_1")

    assert (api.last.method, api.last.path) == ("GET", "/v1/volumes?sandbox_id=sbx_1")
    assert [v.id for v in volumes] == ["vol_1"]


def test_delete_volume_deletes_by_id(api):
    api.answer(204)

    api.client.delete_volume("vol_1")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/volumes/vol_1")


SNAPSHOT = {"id": "snap_1", "name": "after-setup", "sandbox_id": "sbx_1", "image": "busybox:latest",
            "size_bytes": 1024, "created_at": "2026-11-20T10:00:00Z"}


def test_create_snapshot_posts_the_name_under_the_sandbox(api):
    api.answer(201, SNAPSHOT)

    snapshot = api.client.create_snapshot("sbx_1", "after-setup")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/snapshots")
    assert api.last.json == {"name": "after-setup"}
    assert snapshot == Snapshot("snap_1", "after-setup", "sbx_1", 1024, "2026-11-20T10:00:00Z", "busybox:latest")


def test_create_snapshot_raises_conflict_when_the_name_is_in_use(api):
    api.answer(409, {"error": {"code": "already_exists", "message": "a snapshot with this name already exists"}})

    with pytest.raises(ConflictError):
        api.client.create_snapshot("sbx_1", "after-setup")


def test_list_snapshots_decodes_the_snapshots_of_the_tenant(api):
    api.answer(200, {"snapshots": [SNAPSHOT]})

    snapshots = api.client.list_snapshots()

    assert (api.last.method, api.last.path) == ("GET", "/v1/snapshots")
    assert [s.id for s in snapshots] == ["snap_1"]


def test_get_snapshot_fetches_by_id(api):
    api.answer(200, SNAPSHOT)

    snapshot = api.client.get_snapshot("snap_1")

    assert (api.last.method, api.last.path) == ("GET", "/v1/snapshots/snap_1")
    assert snapshot.name == "after-setup"


def test_delete_snapshot_deletes_by_id(api):
    api.answer(204)

    api.client.delete_snapshot("snap_1")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/snapshots/snap_1")


def test_create_sandbox_sends_the_snapshot_to_start_from(api):
    api.answer(201, {"id": "sbx_2", "state": "running"})

    api.client.create_sandbox(snapshot="snap_1")

    assert api.last.json == {"snapshot": "snap_1"}


def test_list_api_keys_decodes_keys_without_secret(api):
    api.answer(200, {"api_keys": [API_KEY, {**API_KEY, "id": "key_2", "revoked_at": "2026-11-20T11:00:00Z"}]})

    keys = api.client.list_api_keys()

    assert (api.last.method, api.last.path) == ("GET", "/v1/api-keys")
    assert keys[0] == ApiKey("key_1", "ci", "lsb_ab12", ["sandbox:execute"], "2026-11-20T10:00:00Z")
    assert (keys[1].revoked_at, keys[1].key) == ("2026-11-20T11:00:00Z", "")


def test_create_api_key_without_scopes_sends_only_the_name(api):
    api.answer(201, {**API_KEY, "key": "lsb_secret"})

    api.client.create_api_key("ci")

    assert (api.last.method, api.last.path) == ("POST", "/v1/api-keys")
    assert api.last.json == {"name": "ci"}


def test_create_api_key_sends_scopes_and_returns_the_secret(api):
    api.answer(201, {**API_KEY, "scopes": ["api_keys:manage"], "key": "lsb_secret"})

    key = api.client.create_api_key("admin", scopes=["api_keys:manage"])

    assert api.last.json == {"name": "admin", "scopes": ["api_keys:manage"]}
    assert key.key == "lsb_secret"


def test_api_key_repr_does_not_show_the_secret(api):
    api.answer(201, {**API_KEY, "key": "lsb_secret"})

    key = api.client.create_api_key("ci")

    assert "lsb_secret" not in repr(key)


def test_revoke_api_key_deletes_by_id(api):
    api.answer(204)

    api.client.revoke_api_key("key_1")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/api-keys/key_1")


def test_list_templates_decodes_builtin_and_registered_entries(api):
    api.answer(200, {"templates": [
        {"name": "python", "image": "python:3.12", "description": "CPython", "builtin": True},
        TEMPLATE,
    ]})

    templates = api.client.list_templates()

    assert (api.last.method, api.last.path) == ("GET", "/v1/templates")
    assert templates[0] == Template(name="python", image="python:3.12", description="CPython", builtin=True)
    assert (templates[1].id, templates[1].builtin, templates[1].scan_status) == ("tpl_1", False, "passed")


def test_register_template_sends_name_and_image_only_by_default(api):
    api.answer(201, TEMPLATE)

    api.client.register_template("ml", "ml@sha256:abc")

    assert (api.last.method, api.last.path) == ("POST", "/v1/templates")
    assert api.last.json == {"name": "ml", "image": "ml@sha256:abc"}


def test_register_template_sends_version_and_public_and_decodes_template(api):
    api.answer(201, TEMPLATE)

    template = api.client.register_template("ml", "ml@sha256:abc", version="1", public=True)

    assert api.last.json == {"name": "ml", "image": "ml@sha256:abc", "version": "1", "public": True}
    assert template == Template(
        name="ml", image="ml@sha256:abc", id="tpl_1", version="1", digest="sha256:abc",
        public=True, scan_status="passed",
    )


def test_get_template_reads_by_id(api):
    api.answer(200, TEMPLATE)

    template = api.client.get_template("tpl_1")

    assert (api.last.method, api.last.path) == ("GET", "/v1/templates/tpl_1")
    assert template.name == "ml"


def test_get_capabilities_decodes_driver_and_entries(api):
    api.answer(200, {"driver": "container", "capabilities": [
        {"name": "fork", "supported": True}, {"name": "pause", "supported": False},
    ]})

    capabilities = api.client.get_capabilities()

    assert (api.last.method, api.last.path) == ("GET", "/v1/capabilities")
    assert capabilities == Capabilities("container", [Capability("fork", True), Capability("pause", False)])


@pytest.mark.parametrize("name, expected", [("fork", True), ("pause", False), ("teleport", False)])
def test_capabilities_supports_is_true_only_for_supported_names(name, expected):
    capabilities = Capabilities("container", [Capability("fork", True), Capability("pause", False)])

    assert capabilities.supports(name) is expected


def test_health_returns_status_without_sending_the_api_key(api):
    api.answer(200, {"status": "ok"})

    status = api.client.health()

    assert (api.last.method, api.last.path) == ("GET", "/v1/healthz")
    assert "Authorization" not in api.last.headers
    assert status == {"status": "ok"}


def test_ready_returns_status_when_dependencies_are_reachable(api):
    api.answer(200, {"status": "ready"})

    status = api.client.ready()

    assert (api.last.method, api.last.path) == ("GET", "/v1/readyz")
    assert status == {"status": "ready"}


def test_ready_raises_server_error_when_a_dependency_is_down(api):
    api.error(503, "not_ready", "dependency unavailable")

    with pytest.raises(ServerError) as raised:
        api.client.ready()

    assert raised.value.code == "not_ready"


def test_prometheus_metrics_returns_the_text_exposition(api):
    api.answer(200, b"# TYPE up gauge\nup 1\n")

    text = api.client.prometheus_metrics()

    assert (api.last.method, api.last.path) == ("GET", "/metrics")
    assert text == "# TYPE up gauge\nup 1\n"


def test_create_sandbox_sends_and_reads_the_idle_limit(api):
    api.answer(201, {"id": "sbx_2", "state": "running", "auto_pause_seconds": 60})

    sandbox = api.client.create_sandbox(auto_pause_seconds=60)

    assert api.last.json == {"auto_pause_seconds": 60}
    assert sandbox.info.auto_pause_seconds == 60


SECRET = {"name": "API_TOKEN", "created_at": "2026-11-20T10:00:00Z", "updated_at": "2026-11-20T11:00:00Z"}


def test_put_secret_sends_the_value_and_returns_the_secret_without_it(api):
    api.answer(201, SECRET)

    secret = api.client.put_secret("API_TOKEN", "v")

    assert (api.last.method, api.last.path) == ("PUT", "/v1/secrets/API_TOKEN")
    assert api.last.json == {"value": "v"}
    assert secret == Secret("API_TOKEN", "2026-11-20T10:00:00Z", "2026-11-20T11:00:00Z")


def test_list_secrets_decodes_the_secrets_of_the_tenant(api):
    api.answer(200, {"secrets": [SECRET]})

    secrets = api.client.list_secrets()

    assert (api.last.method, api.last.path) == ("GET", "/v1/secrets")
    assert [s.name for s in secrets] == ["API_TOKEN"]


def test_delete_secret_deletes_by_name(api):
    api.answer(204)

    api.client.delete_secret("API_TOKEN")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/secrets/API_TOKEN")


def test_create_sandbox_sends_the_names_of_its_secrets(api):
    api.answer(201, {"id": "sbx_2", "state": "running", "secrets": ["API_TOKEN"]})

    sandbox = api.client.create_sandbox(secrets=["API_TOKEN"])

    assert api.last.json == {"secrets": ["API_TOKEN"]}
    assert sandbox.info.secrets == ["API_TOKEN"]
