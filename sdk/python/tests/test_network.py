"""Sandbox network: rules at creation, replacing them, and the refused connections."""
from loopsandbox import NetworkDenial, NetworkPolicy

SANDBOX = {"id": "sbx_1", "tenant_id": "tenant-a", "state": "running"}
RULES = {"allow_out": ["pypi.org"], "deny_out": ["0.0.0.0/0"]}


def test_create_sandbox_sends_the_rules_and_maps_them_back(api):
    api.answer(201, {**SANDBOX, "network": RULES})

    sandbox = api.client.create_sandbox(network=NetworkPolicy(allow_out=["pypi.org"], deny_out=["0.0.0.0/0"]))

    assert api.last.json == {"network": RULES}
    assert sandbox.info.network == NetworkPolicy(allow_out=["pypi.org"], deny_out=["0.0.0.0/0"])


def test_a_sandbox_without_rules_has_no_network(api):
    api.answer(201, SANDBOX)

    sandbox = api.client.create_sandbox()

    assert "network" not in api.last.json
    assert sandbox.info.network is None


def test_update_network_puts_both_lists_and_returns_the_rules_in_force(api):
    api.answer(200, {"allow_out": ["*.example.org"], "deny_out": ["0.0.0.0/0"]})

    applied = api.sandbox().update_network(allow_out=["*.example.org"], deny_out=["0.0.0.0/0"])

    assert (api.last.method, api.last.path) == ("PUT", "/v1/sandboxes/sbx_1/network")
    assert api.last.json == {"allow_out": ["*.example.org"], "deny_out": ["0.0.0.0/0"]}
    assert applied == NetworkPolicy(allow_out=["*.example.org"], deny_out=["0.0.0.0/0"])


def test_update_network_with_no_argument_sends_empty_lists(api):
    api.answer(200, {"allow_out": [], "deny_out": []})

    applied = api.sandbox().update_network()

    assert api.last.json == {"allow_out": [], "deny_out": []}
    assert applied == NetworkPolicy()


def test_network_denials_lists_the_refused_connections(api):
    api.answer(
        200,
        {
            "denials": [
                {"time": "2026-11-27T10:00:00Z", "destination": "93.184.216.34:443", "host": "example.com", "reason": "denied by deny_out"},
                {"time": "2026-11-27T10:00:01Z", "destination": "10.0.0.1:80", "reason": "private address"},
            ]
        },
    )

    denials = api.sandbox().network_denials()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/network/denials")
    assert denials == [
        NetworkDenial(time="2026-11-27T10:00:00Z", destination="93.184.216.34:443", host="example.com", reason="denied by deny_out"),
        NetworkDenial(time="2026-11-27T10:00:01Z", destination="10.0.0.1:80", reason="private address"),
    ]


def test_network_denials_passes_the_limit(api):
    api.answer(200, {"denials": []})

    assert api.sandbox().network_denials(limit=5) == []
    assert api.last.path == "/v1/sandboxes/sbx_1/network/denials?limit=5"
