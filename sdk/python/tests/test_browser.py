"""Browser control."""


def test_browser_navigate_posts_the_url(api):
    api.answer(204)

    api.sandbox().browser_navigate("https://example.com")

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/browser/navigate")
    assert api.last.json == {"url": "https://example.com"}


def test_browser_screenshot_returns_png_bytes(api):
    api.answer(200, b"\x89PNG\r\n")

    png = api.sandbox().browser_screenshot()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/browser/screenshot")
    assert png == b"\x89PNG\r\n"


def test_browser_dom_returns_the_html_string(api):
    api.answer(200, {"dom": "<html></html>"})

    dom = api.sandbox().browser_dom()

    assert (api.last.method, api.last.path) == ("GET", "/v1/sandboxes/sbx_1/browser/dom")
    assert dom == "<html></html>"


def test_browser_input_click_sends_only_the_position(api):
    api.answer(204)

    api.sandbox().browser_input("click", x=10, y=20.5)

    assert (api.last.method, api.last.path) == ("POST", "/v1/sandboxes/sbx_1/browser/input")
    assert api.last.json == {"action": "click", "x": 10, "y": 20.5}


def test_browser_input_type_sends_the_text(api):
    api.answer(204)

    api.sandbox().browser_input("type", text="hello")

    assert api.last.json == {"action": "type", "text": "hello"}


def test_browser_input_scroll_keeps_a_zero_delta(api):
    api.answer(204)

    api.sandbox().browser_input("scroll", x=0, y=0, dx=0, dy=120)

    assert api.last.json == {"action": "scroll", "x": 0, "y": 0, "dx": 0, "dy": 120}
