/* The SAND Vault phone app, when this page is running inside it.

   The Android app (apps/android) shows this same page in a web view, from a
   server on another machine or from one on the phone itself, and adds
   window.sandApp before the page loads (assets/sand-bridge.js). Its own
   settings — which of those two it is, the server's address — live in the
   app, not in the vault, so the page offers a way to them and nothing more.

   In a browser there is no window.sandApp, and nothing here shows. */
export function nativeApp() {
  if (typeof window === 'undefined') return null
  const app = window.sandApp
  return app && typeof app.openSettings === 'function' ? app : null
}

/* What the app is doing, in words for a button's title. */
export function nativeAppMode(app = nativeApp()) {
  if (!app) return ''
  return app.mode === 'on_phone' ? 'The vault is running on this phone' : 'Connected to a vault on another machine'
}
