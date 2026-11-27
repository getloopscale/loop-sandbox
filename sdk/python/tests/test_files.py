"""Files, directories, signed URLs and directory watches."""
from loopsandbox import ArtifactUrl, FileEntry, FileEvent, FileEvents, FileInfo, SignedFileUrl, Watch


def test_write_file_puts_raw_bytes_as_octet_stream(api):
    api.answer(204)

    api.sandbox().write_file("/workspace/a.bin", b"\x00\x01")

    assert (api.last.method, api.last.path) == ("PUT", "/v1/sandboxes/sbx_1/files/workspace/a.bin")
    assert api.last.body == b"\x00\x01"
    assert api.last.headers["Content-Type"] == "application/octet-stream"


def test_write_file_sends_an_empty_file_as_an_empty_body(api):
    api.answer(204)

    api.sandbox().write_file("/workspace/empty", b"")

    assert api.last.body == b""


def test_read_file_returns_raw_bytes(api):
    api.answer(200, b"\xff\xfe binary")

    content = api.sandbox().read_file("/workspace/a.bin")

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/files/workspace/a.bin")
    assert content == b"\xff\xfe binary"


def test_file_path_keeps_slashes_and_encodes_reserved_characters(api):
    api.answer(200, b"")

    api.sandbox().read_file("/workspace/my dir/a#b?.txt")

    assert api.last.path == "/v1/sandboxes/sbx_1/files/workspace/my%20dir/a%23b%3F.txt"


def test_delete_file_sends_delete_to_the_file_route(api):
    api.answer(204)

    api.sandbox().delete_file("/workspace/old.txt")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/sandboxes/sbx_1/files/workspace/old.txt")


def test_list_files_without_path_sends_no_query(api):
    api.answer(200, {"files": []})

    api.sandbox().list_files()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/files")


def test_list_files_sends_directory_as_query(api):
    api.answer(200, {"files": []})

    api.sandbox().list_files("/workspace/src")

    assert api.last.path == "/v1/sandboxes/sbx_1/files?path=%2Fworkspace%2Fsrc"


def test_list_files_decodes_entries(api):
    api.answer(200, {"files": [
        {"name": "a.txt", "size": 3, "is_dir": False},
        {"name": "src", "size": 0, "is_dir": True},
    ]})

    entries = api.sandbox().list_files()

    assert entries == [FileEntry("a.txt", 3, False), FileEntry("src", 0, True)]


def test_file_info_decodes_size_mode_and_modification_time(api):
    api.answer(200, {
        "name": "report.txt", "path": "/workspace/final/report.txt", "size": 12,
        "is_dir": False, "mode": "0644", "modified_at": "2026-11-20T10:00:00Z",
    })

    info = api.sandbox().file_info("/workspace/final/report.txt")

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/file-info/workspace/final/report.txt")
    assert info == FileInfo("report.txt", "/workspace/final/report.txt", 12, False, "0644", "2026-11-20T10:00:00Z")


def test_make_directory_puts_to_the_directories_route_without_body(api):
    api.answer(204)

    api.sandbox().make_directory("/workspace/out/deep")

    assert (api.last.method, api.last.path, api.last.body) == (
        "PUT", "/v1/sandboxes/sbx_1/directories/workspace/out/deep", None,
    )


def test_move_file_sends_from_and_to(api):
    api.answer(204)

    api.sandbox().move_file("/workspace/draft.txt", "/workspace/final/report.txt")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/file-moves")
    assert api.last.json == {"from": "/workspace/draft.txt", "to": "/workspace/final/report.txt"}


def test_move_file_sends_overwrite_when_asked(api):
    api.answer(204)

    api.sandbox().move_file("/workspace/a", "/workspace/b", overwrite=True)

    assert api.last.json == {"from": "/workspace/a", "to": "/workspace/b", "overwrite": True}


def test_create_file_url_sends_path_and_operation(api):
    api.answer(201, {"url": "https://api.test/v1/signed-files/tok", "method": "GET", "expires_at": "2026-11-20T10:05:00Z"})

    signed = api.sandbox().create_file_url("/workspace/report.pdf", "download")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/file-urls")
    assert api.last.json == {"path": "/workspace/report.pdf", "operation": "download"}
    assert signed == SignedFileUrl("https://api.test/v1/signed-files/tok", "GET", "2026-11-20T10:05:00Z")


def test_create_file_url_sends_custom_expiry(api):
    api.answer(201, {"url": "https://api.test/v1/signed-files/tok", "method": "PUT", "expires_at": "2026-11-20T10:01:00Z"})

    api.sandbox().create_file_url("/workspace/in.csv", "upload", expires_in_seconds=60)

    assert api.last.json == {"path": "/workspace/in.csv", "operation": "upload", "expires_in_seconds": 60}


def test_download_signed_file_gets_the_url_without_api_key(api):
    api.answer(200, b"pdf bytes")

    content = api.client.download_signed_file("https://files.test/v1/signed-files/tok")

    assert (api.last.method, api.last.path) == ("GET", "https://files.test/v1/signed-files/tok")
    assert "Authorization" not in api.last.headers
    assert content == b"pdf bytes"


def test_download_signed_file_accepts_a_bare_token(api):
    api.answer(200, b"x")

    api.client.download_signed_file("tok.sig")

    assert api.last.path == "/v1/signed-files/tok.sig"


def test_upload_signed_file_puts_bytes_without_api_key(api):
    api.answer(204)

    api.client.upload_signed_file("https://files.test/v1/signed-files/tok", b"a,b\n")

    assert (api.last.method, api.last.path, api.last.body) == ("PUT", "https://files.test/v1/signed-files/tok", b"a,b\n")
    assert "Authorization" not in api.last.headers


def test_artifact_url_decodes_url_and_lifetime(api):
    api.answer(200, {"url": "https://s3.test/x?sig=1", "expires_in_seconds": 900})

    artifact = api.sandbox().artifact_url("/workspace/out/model.bin")

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/artifact-url/workspace/out/model.bin")
    assert artifact == ArtifactUrl("https://s3.test/x?sig=1", 900)


def test_start_watch_sends_path_and_decodes_watch(api):
    api.answer(201, {"watch_id": "w_1", "path": "/workspace/out", "recursive": False})

    watch = api.sandbox().start_watch("/workspace/out")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/watches")
    assert api.last.json == {"path": "/workspace/out"}
    assert watch == Watch("w_1", "/workspace/out", False)


def test_start_watch_sends_recursive_when_asked(api):
    api.answer(201, {"watch_id": "w_1", "path": "/workspace/out", "recursive": True})

    api.sandbox().start_watch("/workspace/out", recursive=True)

    assert api.last.json == {"path": "/workspace/out", "recursive": True}


def test_watch_events_sends_cursor_and_long_poll_wait(api):
    api.answer(200, {"events": [], "next": 4, "truncated": False, "closed": False})

    api.sandbox().watch_events("w_1", after=4, wait_ms=30000)

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/watches/w_1/events?after=4&wait_ms=30000")


def test_watch_events_decodes_events_and_flags(api):
    api.answer(200, {
        "events": [
            {"type": "create", "path": "/workspace/out/a.txt", "is_dir": False},
            {"type": "overflow", "is_dir": False},
        ],
        "next": 2,
        "truncated": True,
        "closed": True,
    })

    events = api.sandbox().watch_events("w_1")

    assert events == FileEvents(
        [FileEvent("create", False, "/workspace/out/a.txt"), FileEvent("overflow", False, "")],
        next=2, truncated=True, closed=True,
    )


def test_stop_watch_deletes_the_watch(api):
    api.answer(204)

    api.sandbox().stop_watch("w_1")

    assert (api.last.method, api.last.path) == ("DELETE", "/v1/sandboxes/sbx_1/watches/w_1")
