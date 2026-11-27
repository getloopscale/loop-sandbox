"""Failed answers become typed errors."""
import pytest

from loopsandbox import (
    ConflictError,
    ForbiddenError,
    LoopSandboxError,
    NotFoundError,
    OperationTimeoutError,
    PayloadTooLargeError,
    QuotaExceededError,
    RateLimitError,
    ServerError,
    UnauthorizedError,
    UnsupportedError,
    ValidationError,
)


@pytest.mark.parametrize(
    "status, code, expected",
    [
        (400, "invalid_request", ValidationError),
        (401, "unauthorized", UnauthorizedError),
        (402, "quota_exceeded", QuotaExceededError),
        (403, "forbidden", ForbiddenError),
        (404, "not_found", NotFoundError),
        (409, "conflict", ConflictError),
        (413, "payload_too_large", PayloadTooLargeError),
        (422, "scan_failed", ValidationError),
        (429, "rate_limited", RateLimitError),
        (500, "internal_error", ServerError),
        (501, "not_implemented", UnsupportedError),
        (503, "not_ready", ServerError),
        (504, "timeout", OperationTimeoutError),
    ],
)
def test_status_maps_to_typed_error(api, status, code, expected):
    api.error(status, code, "why it failed")

    with pytest.raises(LoopSandboxError) as raised:
        api.client.get_capabilities()

    assert type(raised.value) is expected
    assert (raised.value.status, raised.value.code, raised.value.message) == (status, code, "why it failed")


def test_rate_limit_error_exposes_retry_after_seconds(api):
    api.error(429, "rate_limited", "too many requests for this API key", headers={"Retry-After": "7"})

    with pytest.raises(RateLimitError) as raised:
        api.client.get_capabilities()

    assert raised.value.retry_after_seconds == 7
    assert raised.value.code == "rate_limited"


def test_rate_limit_error_reads_retry_after_header_in_any_case(api):
    api.error(429, "rate_limited", headers={"retry-after": "3"})

    with pytest.raises(RateLimitError) as raised:
        api.client.get_capabilities()

    assert raised.value.retry_after_seconds == 3


def test_rate_limit_error_without_retry_after_header_has_none(api):
    api.error(429, "rate_limited")

    with pytest.raises(RateLimitError) as raised:
        api.client.get_capabilities()

    assert raised.value.retry_after_seconds is None


def test_rate_limit_error_with_unparseable_retry_after_has_none(api):
    api.error(429, "rate_limited", headers={"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"})

    with pytest.raises(RateLimitError) as raised:
        api.client.get_capabilities()

    assert raised.value.retry_after_seconds is None


def test_conflict_error_keeps_the_specific_api_code(api):
    api.error(409, "process_exited", "the process has ended")

    with pytest.raises(ConflictError) as raised:
        api.sandbox().signal_process(7, "TERM")

    assert raised.value.code == "process_exited"


def test_error_body_that_is_not_json_becomes_the_message(api):
    api.answer(502, b"<html>bad gateway</html>")

    with pytest.raises(ServerError) as raised:
        api.client.get_capabilities()

    assert raised.value.code == "error"
    assert raised.value.message == "<html>bad gateway</html>"


def test_error_with_empty_body_still_raises(api):
    api.answer(404)

    with pytest.raises(NotFoundError) as raised:
        api.sandbox().read_file("/workspace/missing.txt")

    assert raised.value.status == 404


def test_error_message_does_not_contain_the_api_key(api):
    api.error(401, "unauthorized", "missing credentials")

    with pytest.raises(UnauthorizedError) as raised:
        api.client.get_capabilities()

    assert "lsb_k" not in str(raised.value)


def test_transport_exception_propagates_unchanged():
    from loopsandbox import LoopSandbox

    def broken_transport(method, url, headers, body):
        raise ConnectionResetError("connection reset by peer")

    client = LoopSandbox("lsb_k", "https://api.test", transport=broken_transport)

    with pytest.raises(ConnectionResetError):
        client.get_capabilities()
