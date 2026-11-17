package com.sandvault.app.core

/**
 * The phone's Back button, asked of the page before the app acts on it.
 *
 * The vault's page keeps the folders you walked through in memory, never in
 * the web view's history (a history entry would outlive locking the vault),
 * so `WebView.canGoBack()` is false for the whole of a visit and Back used to
 * quit the app from anywhere. Now the app first runs [SCRIPT] in the page;
 * assets/sand-bridge.js passes it to the page's handler, which closes a dialog
 * or steps back a folder and answers whether it did (web/src/backButton.js).
 * Only when it did not does the app fall back to the web view's history and
 * then leave.
 */
object BackPress {
    /** Evaluates to true when the page took the press; false on a page without the bridge. */
    const val SCRIPT =
        "(function(){var a=window.sandApp;return !!(a&&typeof a.back==='function'&&a.back()===true)})()"

    /**
     * Whether the page took it, from what `evaluateJavascript` handed back:
     * the result as JSON, so `"true"`, or `"null"` / null when the page could
     * not answer at all — which is treated as not taken, so Back still works.
     */
    fun taken(result: String?): Boolean = result?.trim() == "true"
}
