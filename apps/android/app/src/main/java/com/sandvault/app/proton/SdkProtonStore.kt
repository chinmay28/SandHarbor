package com.sandvault.app.proton

import android.util.Log
import com.sandvault.app.core.proton.AccountCall
import com.sandvault.app.core.proton.BridgeFailure
import com.sandvault.app.core.proton.ProtonApi
import com.sandvault.app.core.proton.ProtonSession
import com.sandvault.app.core.proton.ProtonSignedOut
import com.sandvault.app.core.proton.ProtonStore
import com.sandvault.app.core.proton.ProtonUnauthorized
import com.sandvault.app.core.proton.SessionBook
import com.sandvault.app.core.proton.SessionKeeper
import com.sandvault.app.core.proton.StoredPart
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.filterIsInstance
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import me.proton.drive.sdk.LoggerProvider
import me.proton.drive.sdk.ProtonDriveClient
import me.proton.drive.sdk.ProtonDriveSdk
import me.proton.drive.sdk.ProtonDriveSdkException
import me.proton.drive.sdk.PublicAddressResolver
import me.proton.drive.sdk.UserAddressResolver
import me.proton.drive.sdk.ClientHost
import me.proton.drive.sdk.entity.Address
import me.proton.drive.sdk.entity.ClientCreateRequest
import me.proton.drive.sdk.entity.FileDownloaderRequest
import me.proton.drive.sdk.entity.FileNode
import me.proton.drive.sdk.entity.FileRevisionUploaderRequest
import me.proton.drive.sdk.entity.FileUploaderRequest
import me.proton.drive.sdk.entity.FolderNode
import me.proton.drive.sdk.entity.NodeResultPair
import me.proton.drive.sdk.entity.NodeUid
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.nio.channels.Channels
import java.time.OffsetDateTime
import java.util.concurrent.ConcurrentHashMap

/**
 * Proton Drive, through Proton's own Drive SDK, as the bridge's [ProtonStore]:
 * one folder of parts per account.
 *
 * One SDK client per Proton session UID, built on the first request after the
 * app starts — which fetches and unlocks the account's keys, the slow part —
 * and kept while the app runs. The session it uses is whichever is newest in
 * the [SessionBook]; a refresh any request makes reaches every other.
 */
class SdkProtonStore(
    private val api: ProtonApi,
    private val baseUrl: String = ProtonApi.DEFAULT_BASE_URL,
) : ProtonStore {
    private val book = SessionBook()
    private val keeper = SessionKeeper(book) { api.refresh(it) }
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val accounts = ConcurrentHashMap<String, Account>()
    private val building = Mutex()

    /** One signed-in account: its SDK client, its unlocked keys, and what it has learned of its folders. */
    private inner class Account(val uid: String, val client: ProtonDriveClient, val keys: UnlockedAccount) {
        val folders = ConcurrentHashMap<String, NodeUid>()
        /** Per folder: part name to its node, filled by a listing and kept up to date by writes. */
        val parts = ConcurrentHashMap<String, ConcurrentHashMap<String, FileNode>>()
        val folderLock = Mutex()
    }

    // --- ProtonStore ---------------------------------------------------------

    override fun ping(call: AccountCall) = run(call) { a -> folderOf(a, call.folder); Unit }

    override fun email(call: AccountCall): String = run(call) { _ ->
        call.session.email.ifBlank { withSession(call) { api.user(it).email } }
    }

    override fun usage(call: AccountCall): Pair<Long, Long> = run(call) { _ ->
        withSession(call) { api.user(it) }.let { it.usedSpace to it.maxSpace }
    }

    override fun list(call: AccountCall, prefix: String): List<StoredPart> = run(call) { a ->
        partsOf(a, call.folder, fresh = true).values
            .filter { it.name.getOrNull()?.startsWith(prefix) == true }
            .map { StoredPart(it.name.getOrThrow(), sizeOf(it)) }
    }

    override fun stat(call: AccountCall, key: String): StoredPart = run(call) { a ->
        val node = find(a, call.folder, key)
        StoredPart(key, sizeOf(node))
    }

    override fun get(call: AccountCall, key: String): ByteArray = run(call) { a ->
        val node = find(a, call.folder, key)
        val out = ByteArrayOutputStream(sizeOf(node).toInt().coerceIn(0, 64 shl 20))
        a.client.downloader(FileDownloaderRequest(node.activeRevision.uid)).use { downloader ->
            downloader.downloadToStream(scope, Channels.newChannel(out)).awaitCompletion()
        }
        out.toByteArray()
    }

    override fun put(call: AccountCall, key: String, data: ByteArray) = run(call) { a ->
        val folder = folderOf(a, call.folder)
        val existing = partsOf(a, call.folder, fresh = false)[key]
        val now = OffsetDateTime.now()
        val uploader = if (existing != null) {
            a.client.uploader(FileRevisionUploaderRequest(existing.activeRevision.uid, now, data.size.toLong()))
        } else {
            a.client.uploader(
                FileUploaderRequest(
                    parentFolderUid = folder,
                    name = key,
                    mediaType = "application/octet-stream",
                    fileSize = data.size.toLong(),
                    lastModificationTime = now,
                    // A part whose upload was cut short last time is redone, not refused.
                    overrideExistingDraftByOtherClient = true,
                ),
            )
        }
        uploader.use { up ->
            up.uploadFromStream(scope, Channels.newChannel(ByteArrayInputStream(data))).awaitCompletion()
        }
        // The new node's details come with the next listing.
        a.parts[call.folder]?.remove(key)
        Unit
    }

    override fun delete(call: AccountCall, key: String) = run(call) { a ->
        val node = find(a, call.folder, key)
        // Proton deletes from the trash; a part goes there and straight out.
        failures(a.client.trashNodes(listOf(node.uid)).toList())
        failures(a.client.deleteNodes(listOf(node.uid)).toList())
        a.parts[call.folder]?.remove(key)
        Unit
    }

    // --- the account and its folder -----------------------------------------

    /**
     * [body] against the account [call] names, with the failures the bridge
     * understands, and any refreshed session handed back through [call].
     */
    private fun <T> run(call: AccountCall, body: suspend (Account) -> T): T = runBlocking(Dispatchers.IO) {
        val session = book.resolve(call.session)
        try {
            val result = body(account(session))
            book.current(session.uid)?.takeIf { it != call.session }?.let { call.refreshed = it }
            result
        } catch (e: ProtonDriveSdkException) {
            Log.w(TAG, "Proton: ${e.message}")
            book.current(session.uid)?.takeIf { it != call.session }?.let { call.refreshed = it }
            throw if (isUnauthorized(e)) BridgeFailure.SignedOut("Proton no longer accepts this sign-in") else BridgeFailure.Proton(e.message ?: "Proton Drive failed")
        } catch (e: ProtonSignedOut) {
            accounts.remove(session.uid)?.client?.close()
            throw e
        }
    }

    private fun <T> withSession(call: AccountCall, fn: (ProtonSession) -> T): T =
        keeper.run(call.session, fn).let { (result, used) ->
            if (used != call.session) call.refreshed = used
            result
        }

    private suspend fun account(session: ProtonSession): Account {
        accounts[session.uid]?.let { return it }
        return building.withLock {
            accounts[session.uid] ?: create(session).also { accounts[session.uid] = it }
        }
    }

    private suspend fun create(session: ProtonSession): Account {
        val (user, used) = keeper.run(session) { api.user(it) }
        val (addresses, _) = keeper.run(used) { api.addresses(it) }
        val keys = UnlockedAccount.unlock(user, addresses, used.keyPassword)

        val logger = ProtonDriveSdk.loggerProvider { level, category, message ->
            if (level >= LoggerProvider.Level.WARN) Log.w(TAG, "$category: $message")
        }
        val host = ClientHost(
            userId = user.id.ifBlank { session.uid },
            httpClient = SessionHttpClient(api, keeper, current = { book.current(session.uid) ?: used }),
            userAddressResolver = object : UserAddressResolver {
                override suspend fun getAddress(id: String): Address = keys.address(id)
                override suspend fun getDefaultAddress(): Address = keys.defaultAddress()
                override suspend fun <T> getAddressPrimaryPrivateKey(id: String, block: (ByteArray) -> T): T = block(keys.primaryPrivateKey(id))
                override suspend fun <T> getAddressPrivateKeys(id: String, block: (List<ByteArray>) -> T): T = block(keys.privateKeys(id))
            },
            publicAddressResolver = object : PublicAddressResolver {
                override suspend fun getAddressPublicKeys(emailAddress: String): List<ByteArray> =
                    keys.ownPublicKeys(emailAddress) ?: keeper.run(book.current(session.uid) ?: used) { api.publicKeys(it, emailAddress) }
                        .first.mapNotNull { armored -> runCatching { com.proton.gopenpgp.crypto.Crypto.newKeyFromArmored(armored).publicKey }.getOrNull() }
            },
        )
        val client = ProtonDriveSdk.protonDriveClientCreate(
            coroutineScope = scope,
            host = host,
            request = ClientCreateRequest(
                baseUrl = baseUrl,
                loggerProvider = logger,
                // Nothing on disk: the vault is where this account's secrets live.
                cachePath = null,
                uid = session.uid,
            ),
        )
        return Account(session.uid, client, keys)
    }

    /** The folder the parts go in, made — every missing level of it — if it is not there. */
    private suspend fun folderOf(a: Account, path: String): NodeUid {
        a.folders[path]?.let { return it }
        return a.folderLock.withLock {
            a.folders[path] ?: walk(a, path).also { a.folders[path] = it }
        }
    }

    private suspend fun walk(a: Account, path: String): NodeUid {
        val segments = path.trim('/').split('/').filter { it.isNotEmpty() }
        if (segments.firstOrNull() != MY_FILES) {
            throw BridgeFailure.BadRequest("the folder must be under /$MY_FILES, where the account's own files are: $path")
        }
        var here: NodeUid = a.client.getMyFilesFolder().uid
        for (name in segments.drop(1)) {
            val existing = children(a, here).filterIsInstance<FolderNode>().firstOrNull { it.name.getOrNull() == name }
            here = existing?.uid ?: a.client.createFolder(here, name).uid
        }
        return here
    }

    private suspend fun children(a: Account, folder: NodeUid): List<me.proton.drive.sdk.entity.Node> {
        val uids = a.client.enumerateFolderChildrenNodeUids(folder).toList()
        if (uids.isEmpty()) return emptyList()
        return uids.chunked(NODE_BATCH).flatMap { a.client.enumerateNodes(it).toList() }.filter { it.trashTime == null }
    }

    private suspend fun partsOf(a: Account, path: String, fresh: Boolean): Map<String, FileNode> {
        if (!fresh) a.parts[path]?.let { return it }
        val folder = folderOf(a, path)
        val found = ConcurrentHashMap<String, FileNode>()
        children(a, folder).filterIsInstance<FileNode>().forEach { node ->
            node.name.getOrNull()?.let { found[it] = node }
        }
        a.parts[path] = found
        return found
    }

    private suspend fun find(a: Account, path: String, key: String): FileNode =
        partsOf(a, path, fresh = false)[key]
            ?: partsOf(a, path, fresh = true)[key]
            ?: throw BridgeFailure.NotFound("no part $key in $path")

    private fun sizeOf(node: FileNode): Long = node.activeRevision.claimedSize ?: node.activeRevision.storageSize

    private fun failures(results: List<NodeResultPair>) {
        results.filterIsInstance<NodeResultPair.Failure>().firstOrNull()?.let { throw it.error }
    }

    private fun isUnauthorized(e: Throwable?): Boolean {
        var t = e
        while (t != null) {
            if (t is ProtonUnauthorized || t is ProtonSignedOut) return true
            if (t is me.proton.drive.sdk.SdkHttpClientException.Http && t.statusCode == 401) return true
            t = t.cause
        }
        return false
    }

    private companion object {
        const val TAG = "SandProton"
        const val MY_FILES = "my-files"
        const val NODE_BATCH = 100
    }
}
