// The version an app is built as, for both Android apps (SAND Vault and
// SAND TV): the same vYEAR.MONTH.PATCH a release carries, on every build, not
// just the ones the release workflow stamps. Applied from each app's
// build.gradle.kts, which reads the three values back from `extra`:
//
//   sandVersionName  v2026.11.246  the version, as releases and the server say it
//   sandVersionCode  246           the commit count; Android's versionCode
//   sandVersionDate  2026-11       the commit's month, for the server's version.Date
//
// -PsandVersionName / -PsandVersionCode / -PsandVersionDate still win, so a
// release is stamped exactly as before. Without them the values come from
// scripts/version.mjs, the one place the scheme is worked out — the web build
// and `make build` ask it too, so an APK, its server and the page it serves
// all agree.
//
// A shallow clone has no real commit count, and version.mjs answers patch 0
// for it rather than a plausible lie; versionCode, which Android needs to be
// at least 1, is then 1. With no node at all (an IDE that does not have it on
// its PATH) the build still works, as "dev".

val repoRoot = rootProject.projectDir.resolve("../..").canonicalFile

fun stamped(name: String): String? = (findProperty(name) as String?)?.trim()?.takeIf { it.isNotEmpty() }

val asked: List<String>? by lazy {
    runCatching {
        val out = providers.exec {
            workingDir = repoRoot
            commandLine("node", "scripts/version.mjs", "--all")
            isIgnoreExitValue = true
        }
        if (out.result.get().exitValue != 0) null
        else out.standardOutput.asText.get().lines().map { it.trim() }.filter { it.isNotEmpty() }.takeIf { it.size == 3 }
    }.getOrNull()
}

val versionName = stamped("sandVersionName") ?: asked?.get(0) ?: "dev"
val versionCode = stamped("sandVersionCode") ?: asked?.get(1) ?: "1"
val versionDate = stamped("sandVersionDate") ?: asked?.get(2) ?: ""

if (asked == null && stamped("sandVersionName") == null) {
    logger.warn("sand-version: could not run `node scripts/version.mjs`; building as \"dev\"")
}

extra["sandVersionName"] = versionName
extra["sandVersionCode"] = (versionCode.toIntOrNull() ?: 0).coerceAtLeast(1)
// The server's own patch is the count as it is, 0 included: patch 0 is how a
// sand binary says it is an unstamped build, and it should say so here too.
extra["sandServerPatch"] = versionCode
extra["sandVersionDate"] = versionDate
