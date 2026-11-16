package com.sandvault.app.core.proton

import org.json.JSONObject
import java.net.URLEncoder
import java.security.SecureRandom
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/**
 * Proton's sign-in by link for third-party clients — a *session fork* — on
 * the app's side. The same exchange Proton's own CLI makes
 * (sdk/incubating/account/js/src/authWeb.ts):
 *
 *  1. the app asks Proton for a fork, and is given a user code and a selector;
 *  2. it builds a link to Proton's sign-in page carrying the code and a key
 *     only the app knows ([link]);
 *  3. somebody follows it, signs in and approves;
 *  4. the app's poll of the selector then answers with a session and a payload
 *     the page encrypted under that key, holding the account's key password
 *     ([keyPassword]).
 *
 * The app never sees a password, a second factor or a CAPTCHA.
 */
object ForkSignIn {
    /** What a third-party client calls itself in the link (the CLI's own rule). */
    const val CLIENT_ID = "external-drive"

    const val ACCOUNT_HOST = "account.proton.me"

    private const val NONCE = 12
    private const val TAG = 16
    private val AAD = "fork".toByteArray(Charsets.UTF_8)

    /** 32 fresh random bytes: the key the page encrypts the key password under. */
    fun newKey(random: SecureRandom = SecureRandom()): ByteArray = ByteArray(32).also(random::nextBytes)

    fun link(userCode: String, key: ByteArray, accountHost: String = ACCOUNT_HOST, clientId: String = CLIENT_ID): String {
        require(userCode.isNotBlank()) { "Proton gave no user code" }
        require(key.size == 32) { "the fork key must be 32 bytes" }
        val payload = "0:$userCode:${Base64.getEncoder().encodeToString(key)}:$clientId"
        // encodeURIComponent, which URLEncoder is apart from writing spaces as +.
        val encoded = URLEncoder.encode(payload, "UTF-8").replace("+", "%20")
        return "https://$accountHost/desktop/login?app=drive&pv=3#payload=$encoded"
    }

    /** The key password out of a fork's payload: AES-256-GCM, nonce first, tag last, AAD "fork". */
    fun keyPassword(key: ByteArray, payload: String): Result<String> = runCatching {
        val blob = Base64.getDecoder().decode(payload.trim())
        require(blob.size >= NONCE + TAG) { "the sign-in payload is too short" }
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.DECRYPT_MODE, SecretKeySpec(key, "AES"), GCMParameterSpec(TAG * 8, blob, 0, NONCE))
        cipher.updateAAD(AAD)
        val plain = String(cipher.doFinal(blob, NONCE, blob.size - NONCE), Charsets.UTF_8)
        val password = JSONObject(plain).optString("keyPassword")
        require(password.isNotEmpty()) { "the sign-in payload holds no key password" }
        password
    }

    /** The other half, for tests: what Proton's page does. */
    internal fun seal(key: ByteArray, keyPassword: String, nonce: ByteArray = ByteArray(NONCE).also(SecureRandom()::nextBytes)): String {
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.ENCRYPT_MODE, SecretKeySpec(key, "AES"), GCMParameterSpec(TAG * 8, nonce))
        cipher.updateAAD(AAD)
        val sealed = cipher.doFinal(JSONObject().put("type", "default").put("keyPassword", keyPassword).toString().toByteArray())
        return Base64.getEncoder().encodeToString(nonce + sealed)
    }
}

/**
 * The value of `x-pm-appversion`, which Proton requires every client to send
 * and to be honest in: `external-drive-{name}@{semver}-{channel}`.
 */
object ProtonAppVersion {
    const val NAME = "sand_vault"

    /** From the app's own version name (v2026.11.247, or "dev" for an unstamped build). */
    fun of(versionName: String): String {
        val m = Regex("""^v?(\d+)\.(\d+)\.(\d+)""").find(versionName.trim())
        return if (m == null) {
            "external-drive-$NAME@0.0.0-alpha"
        } else {
            val (major, minor, patch) = m.destructured
            // Patch 0 is how SAND marks an unstamped build; that is not a release.
            val channel = if (patch == "0") "alpha" else "stable"
            "external-drive-$NAME@${major.toInt()}.${minor.toInt()}.${patch.toInt()}-$channel"
        }
    }
}
