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
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
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
}
