package com.sandvault.app.proton

import com.proton.gopenpgp.crypto.Crypto
import com.proton.gopenpgp.crypto.Key
import com.proton.gopenpgp.crypto.KeyRing
import com.sandvault.app.core.proton.AddressKey
import com.sandvault.app.core.proton.BridgeFailure
import com.sandvault.app.core.proton.ProtonAddress
import com.sandvault.app.core.proton.ProtonUser
import me.proton.drive.sdk.entity.Address

/**
 * The account's address keys, unlocked, as the Drive SDK asks for them.
 *
 * Proton's chain, as its own account module walks it
 * (sdk/incubating/account/js/src/addresses.ts): the key password from the
 * sign-in unlocks the user's keys; each address key's passphrase is a token
 * encrypted to the user key and signed by it — checked — or, on an account old
 * enough not to have tokens, the key password itself. The SDK takes each
 * unlocked key as its serialized OpenPGP bytes.
 *
 * Held in memory for as long as the account's client is, and never written
 * anywhere.
 */
class UnlockedAccount private constructor(
    val addresses: List<ProtonAddress>,
    private val unlocked: Map<String, List<Pair<AddressKey, ByteArray>>>,
    private val publicKeys: Map<String, List<ByteArray>>,
) {
    fun address(id: String): Address = toSdk(addresses.firstOrNull { it.id == id } ?: throw BridgeFailure.Proton("no address $id on this account"))

    fun defaultAddress(): Address = toSdk(
        addresses.firstOrNull { it.status == 1 && unlocked[it.id].orEmpty().isNotEmpty() }
            ?: throw BridgeFailure.Proton("this Proton account has no address whose key could be unlocked"),
    )

    fun primaryPrivateKey(addressId: String): ByteArray {
        val keys = unlocked[addressId].orEmpty()
        return (keys.firstOrNull { it.first.primary } ?: keys.firstOrNull())?.second
            ?: throw BridgeFailure.Proton("no unlocked key for address $addressId")
    }

    fun privateKeys(addressId: String): List<ByteArray> = unlocked[addressId].orEmpty().map { it.second }

    /** Our own address's public keys, or null when [email] is somebody else's. */
    fun ownPublicKeys(email: String): List<ByteArray>? =
        addresses.firstOrNull { it.email.equals(email, ignoreCase = true) }?.let { publicKeys[it.id].orEmpty() }

    private fun toSdk(a: ProtonAddress): Address {
        val keys = unlocked[a.id].orEmpty()
        return Address(
            addressId = a.id,
            order = a.order,
            emailAddress = a.email,
            status = when (a.status) {
                0 -> Address.Status.DISABLED
                2 -> Address.Status.DELETING
                else -> Address.Status.ENABLED
            },
            keys = keys.map { (k, _) ->
                Address.Key(
                    addressId = a.id,
                    keyId = k.id,
                    active = k.active,
                    allowedForEncryption = k.flags and 2 != 0,
                    allowedForVerification = k.flags and 1 != 0,
                )
            },
            primaryKeyIndex = keys.indexOfFirst { it.first.primary }.coerceAtLeast(0),
        )
    }

    companion object {
        fun unlock(user: ProtonUser, addresses: List<ProtonAddress>, keyPassword: String): UnlockedAccount {
            val password = keyPassword.toByteArray(Charsets.UTF_8)
            val userKeys = user.keys.filter { it.active }.mapNotNull { k ->
                runCatching { Crypto.newKeyFromArmored(k.privateKey).unlock(password) }.getOrNull()
            }
            if (userKeys.isEmpty()) throw BridgeFailure.SignedOut("the key password from the sign-in does not unlock this account's keys — sign in again")
            val userRing = ringOf(userKeys)

            val unlocked = HashMap<String, List<Pair<AddressKey, ByteArray>>>()
            val publics = HashMap<String, List<ByteArray>>()
            for (address in addresses) {
                val keys = address.keys.filter { it.active }.mapNotNull { k ->
                    runCatching { k to unlockAddressKey(k, userRing, password) }.getOrNull()
                }
                unlocked[address.id] = keys.map { (k, key) -> k to key.serialize() }
                publics[address.id] = keys.map { (_, key) -> key.publicKey }
            }
            return UnlockedAccount(addresses, unlocked, publics)
        }

        private fun unlockAddressKey(k: AddressKey, userRing: KeyRing, keyPassword: ByteArray): Key {
            val locked = Crypto.newKeyFromArmored(k.privateKey)
            if (k.token.isBlank()) return locked.unlock(keyPassword)
            val token = userRing.decrypt(Crypto.newPGPMessageFromArmored(k.token), null, 0)
            // The token must be the user key's own, not merely readable by it.
            userRing.verifyDetached(token, Crypto.newPGPSignatureFromArmored(k.signature), Crypto.getUnixTime())
            return locked.unlock(token.binary)
        }

        private fun ringOf(keys: List<Key>): KeyRing = Crypto.newKeyRing(keys.first()).also { ring ->
            keys.drop(1).forEach(ring::addKey)
        }
    }
}
