"""
The page as the SAND Vault Android app shows it (apps/android).

The app shows this same web interface in a web view and adds one script,
apps/android/app/src/main/assets/sand-bridge.js, which turns the page's
downloads and navigator.share() into messages to native code over an object
the web view injects, `SandAndroid`. These tests load the real page with the
real script and play the native side in JavaScript, so a change to how the
page saves or shares a file that the app would no longer catch fails here —
without a phone.

The native side's half of the contract is apps/android/core (Bridge.kt and
Transfers.kt), tested on its own.
"""
import os

import pytest

from conftest import PROJECT_ROOT

pytestmark = pytest.mark.gui

BRIDGE_JS = os.path.join(PROJECT_ROOT, "apps", "android", "app", "src", "main", "assets", "sand-bridge.js")

# What the web view's injected object does, and what the app does with each
# message, reduced to recording it. Chunks are reassembled the way
# Transfers.kt does: in order, decoded from base64.
FAKE_NATIVE = r"""
(() => {
  const native = window.__native = { messages: [], files: {}, shared: [], fetched: [], settings: 0 }
  const listeners = []
  let n = 0
  const reply = (o) => listeners.forEach((fn) => fn({ data: JSON.stringify(o) }))
  const handle = (m) => {
    switch (m.t) {
      case 'begin': {
        const id = 't' + (++n)
        native.files[id] = { name: m.name, mime: m.mime, size: m.size, action: m.action, chunks: [] }
        return reply({ re: m.re, ok: true, id })
      }
      case 'chunk': {
        const f = native.files[m.id]
        if (m.seq !== f.chunks.length) return reply({ re: m.re, ok: false, error: 'out of order' })
        f.chunks.push(atob(m.data))
        return reply({ re: m.re, ok: true })
      }
      case 'end': {
        const f = native.files[m.id]
        f.data = f.chunks.join('')
        f.done = f.data.length === f.size
        return reply({ re: m.re, ok: f.done, id: m.id, name: f.name, error: 'short' })
      }
      case 'share': native.shared.push(m); return reply({ re: m.re, ok: true })
      case 'fetch': native.fetched.push(m); return reply({ re: m.re, ok: true })
      case 'settings': native.settings++; return reply({ re: m.re, ok: true })
      default: return undefined
    }
  }
  window.SandAndroid = {
    postMessage(text) { const m = JSON.parse(text); native.messages.push(m); setTimeout(() => handle(m), 0) },
    addEventListener(type, fn) { if (type === 'message') listeners.push(fn) },
  }
})()
"""


def _bridge_script(mode="on_phone"):
    with open(BRIDGE_JS, encoding="utf-8") as f:
        js = f.read()
    assert '"__SAND_MODE__"' in js, "PageBridge.kt replaces this placeholder; keep it"
    return js.replace('"__SAND_MODE__"', f'"{mode}"')


@pytest.fixture
def app_page(page, spawn_server, frontend_built):
    if not frontend_built:
        pytest.skip("the React frontend is not built")
    base = spawn_server("android-app")
    page.add_init_script(FAKE_NATIVE)
    page.add_init_script(_bridge_script())
    page.goto(base)
    page.get_by_text("Create your vault").wait_for(timeout=30000)
    return page, base


def test_the_page_knows_it_is_in_the_app(app_page):
    page, _ = app_page
    assert page.evaluate("window.sandApp && window.sandApp.platform") == "android"
    assert page.evaluate("window.sandApp.mode") == "on_phone"
    assert page.evaluate("navigator.canShare({ files: [new File(['x'], 'x.txt')] })") is True


def test_the_lock_screen_offers_the_app_settings_and_every_way_in(app_page):
    page, _ = app_page
    # A phone with no vault yet: create one, or restore one from a kit —
    # which is also how the phone joins a vault running elsewhere.
    page.get_by_text("I have a recovery kit").wait_for()
    page.get_by_text("⚙ App settings").click()
    page.wait_for_function("window.__native.settings === 1")

    # The kit's own dialog — where "the vault is still running on another
    # machine" is ticked once a kit is chosen — opens in the app as in a browser.
    page.get_by_text("I have a recovery kit").click()
    page.get_by_text("Import a recovery kit").first.wait_for()


def test_saving_a_blob_reaches_the_app_in_order(app_page):
    page, _ = app_page
    # Big enough for several chunks, and bytes that are not text.
    result = page.evaluate("""async () => {
      const bytes = new Uint8Array(2 * 1024 * 1024 + 123)
      for (let i = 0; i < bytes.length; i++) bytes[i] = (i * 7919) % 251
      const url = URL.createObjectURL(new Blob([bytes], { type: 'application/pdf' }))
      const a = document.createElement('a')
      a.href = url
      a.download = 'passport.pdf'
      document.body.appendChild(a)
      a.click()
      a.remove()
      const deadline = Date.now() + 15000
      let f
      while (Date.now() < deadline) {
        f = Object.values(window.__native.files).find((x) => x.done)
        if (f) break
        await new Promise((r) => setTimeout(r, 50))
      }
      if (!f) return { error: 'never arrived' }
      let same = f.data.length === bytes.length
      for (let i = 0; same && i < bytes.length; i++) same = f.data.charCodeAt(i) === bytes[i]
      return { name: f.name, mime: f.mime, action: f.action, chunks: f.chunks.length, same }
    }""")
    assert result == {"name": "passport.pdf", "mime": "application/pdf", "action": "save", "chunks": 3, "same": True}


def test_a_download_from_the_server_is_fetched_by_the_app(app_page):
    page, base = app_page
    page.evaluate("""() => {
      const a = document.createElement('a')
      a.href = '/api/folders/zip?token=abc'
      a.download = 'Holiday.zip'
      a.click()
    }""")
    page.wait_for_function("window.__native.fetched.length === 1")
    fetched = page.evaluate("window.__native.fetched[0]")
    assert fetched["url"] == base + "/api/folders/zip?token=abc"
    assert fetched["name"] == "Holiday.zip"


def test_share_stages_every_file_then_opens_the_sheet(app_page):
    page, _ = app_page
    page.evaluate("""() => navigator.share({
      title: 'Two files',
      files: [new File(['first'], 'a.txt', { type: 'text/plain' }), new File([''], 'empty.bin')],
    })""")
    shared = page.evaluate("window.__native.shared")
    files = page.evaluate("window.__native.files")
    assert len(shared) == 1
    assert shared[0]["title"] == "Two files"
    assert [files[i]["name"] for i in shared[0]["ids"]] == ["a.txt", "empty.bin"]
    assert all(files[i]["action"] == "stage" and files[i]["done"] for i in shared[0]["ids"])
    assert files[shared[0]["ids"][0]]["data"] == "first"


def test_the_header_offers_the_app_settings_once_unlocked(app_page, vault_password):
    page, _ = app_page
    boxes = page.locator('input[autocomplete="new-password"]')
    boxes.nth(0).fill(vault_password)
    boxes.nth(1).fill(vault_password)
    page.get_by_text("▶ Create vault").click()
    page.get_by_role("button", name="App settings").click()
    page.wait_for_function("window.__native.settings === 1")


def test_a_plain_browser_sees_none_of_it(page, spawn_server, frontend_built):
    if not frontend_built:
        pytest.skip("the React frontend is not built")
    page.goto(spawn_server("plain-browser"))
    page.get_by_text("Create your vault").wait_for(timeout=30000)
    assert page.evaluate("window.sandApp === undefined")
    assert page.get_by_text("⚙ App settings").count() == 0
