plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// The vault that runs on the phone in full-stack mode is the real `sand`
// binary, built from this repository by the Go toolchain and packaged as a
// native library (libsand.so), which is the one place Android lets an app
// keep a file it may execute.
//
//   -PsandServer=false            build a thin client only, without Go
//   -PsandServerAbis=arm64-v8a,x86_64
//                                 which ABIs get a server (x86_64 is for the
//                                 emulator); arm64-v8a by default
//   -PsandGo=/path/to/go          the go command, if it is not on PATH
val repoRoot = rootProject.projectDir.resolve("../..").canonicalFile

// vYEAR.MONTH.PATCH from git on every build, or the release's -P values.
apply(from = rootProject.file("../sand-version.gradle.kts"))
val sandVersionName = extra["sandVersionName"] as String
val sandVersionCode = extra["sandVersionCode"] as Int
val sandServerPatch = extra["sandServerPatch"] as String
val sandVersionDate = extra["sandVersionDate"] as String
val withServer = (findProperty("sandServer") as String?) != "false"
val serverAbis = ((findProperty("sandServerAbis") as String?) ?: "arm64-v8a")
    .split(',').map { it.trim() }.filter { it.isNotEmpty() }
val goCommand = (findProperty("sandGo") as String?) ?: "go"
val serverLibs = layout.buildDirectory.dir("generated/sandServer/jniLibs")

// GOOS=android where Go supports it without cgo (arm64), which keeps Android's
// seccomp filter happy and reads the system's time zone database. The
// emulator's x86_64 has no cgo-free android port, so it gets a static linux
// binary with the zone database built in. Both get DNS and the zone from the
// app — see internal/hostenv.
data class GoTarget(val goos: String, val goarch: String, val tags: String)
val goTargets = mapOf(
    "arm64-v8a" to GoTarget("android", "arm64", ""),
    "x86_64" to GoTarget("linux", "amd64", "timetzdata"),
)

android {
    namespace = "com.sandvault.app"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.sandvault.app"
        // Android 10: scoped storage and MediaStore.Downloads, so saving a
        // file needs no storage permission.
        minSdk = 29
        targetSdk = 35
        // The release's version as the name and its commit count as the
        // code — on every build, not only the release workflow's; see
        // apps/sand-version.gradle.kts.
        versionCode = sandVersionCode
        versionName = sandVersionName
        buildConfigField("boolean", "HAS_SERVER", withServer.toString())
    }

    buildFeatures {
        buildConfig = true
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // Signed with the debug key so the release build sideloads as it
            // stands. Anybody publishing to a store signs it with their own.
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    sourceSets["main"].jniLibs.srcDir(serverLibs)

    packaging {
        jniLibs {
            // Installed as a file on disk, not mapped from inside the APK:
            // the server is exec'd, not loaded.
            useLegacyPackaging = true

            // Proton Drive is reached through the app only for the vault on
            // the phone, which exists only where there is a server. Proton's
            // SDK and crypto libraries for every other ABI would be ~45 MB
            // that a thin client on a 32-bit phone carries and never loads.
            val protonLibs = listOf(
                "libproton_drive_sdk.so", "libproton_drive_sdk_jni.so", "libgojni.so",
                "libcrypto.so", "libssl.so", "libe_sqlite3.so",
            )
            val serverless = listOf("armeabi-v7a", "x86", "arm64-v8a", "x86_64")
                .filter { !withServer || it !in serverAbis }
            for (abi in serverless) for (lib in protonLibs) excludes += "lib/$abi/$lib"
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    lint {
        disable += setOf(
            // Plain HTTP is how SAND runs on a home network and on 127.0.0.1,
            // and a private CA is how it runs behind TLS at home: both are
            // the point (see network_security_config.xml).
            "InsecureBaseConfiguration",
            "AcceptsUserCertificates",
            // allowBackup is off; there is nothing for fullBackupContent to say.
            "DataExtractionRules",
            // Versions move on purpose, together, not whenever lint notices.
            "GradleDependency",
        )
    }
}

if (withServer) {
    // The server in the APK reports the version the APK does.
    val version = sandServerPatch
    val date = sandVersionDate
    val versionPkg = "github.com/chinmay28/SandHarbor/internal/version"
    val buildAll = tasks.register("buildSandServer")
    for (abi in serverAbis) {
        val target = goTargets[abi] ?: throw GradleException("no Go target for ABI $abi (known: ${goTargets.keys})")
        val out = serverLibs.map { it.file("$abi/libsand.so") }
        val task = tasks.register<Exec>("buildSandServer-$abi") {
            description = "Builds sand for $abi as libsand.so"
            workingDir = repoRoot
            inputs.files(fileTree(repoRoot) {
                include("go.mod", "go.sum", "cmd/**/*.go", "internal/**")
                exclude("**/*_test.go")
            })
            inputs.property("version", version)
            inputs.property("date", date)
            outputs.file(out)
            environment("CGO_ENABLED", "0")
            environment("GOOS", target.goos)
            environment("GOARCH", target.goarch)
            val args = mutableListOf(goCommand, "build", "-trimpath")
            if (target.tags.isNotEmpty()) args += listOf("-tags", target.tags)
            args += listOf(
                "-ldflags=-s -w -X $versionPkg.Patch=$version -X $versionPkg.Date=$date",
                "-o", out.get().asFile.absolutePath,
                "./cmd/sand",
            )
            commandLine(args)
        }
        buildAll.configure { dependsOn(task) }
    }
    tasks.named("preBuild") { dependsOn(buildAll) }
}

dependencies {
    implementation(project(":core"))
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-ktx:1.9.3")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.preference:preference-ktx:1.2.1")
    implementation("androidx.webkit:webkit:1.12.1")
    implementation("com.google.android.material:material:1.12.0")

    // Proton Drive for the vault on the phone (proton/ in this module, and
    // docs/design/proton-android.md): Proton's own Drive SDK, and its crypto
    // library, which unlocks the account's keys. GPLv3, which the AGPL that
    // SAND is under can carry. Personal, non-commercial use, as Proton's
    // terms for third-party clients require.
    implementation("me.proton.drive:sdk:0.33.0")
    implementation("me.proton.crypto:android-golib:2.10.0-2")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
    // The SDK's HTTP is the host's to supply; the same OkHttp the SDK uses.
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}
