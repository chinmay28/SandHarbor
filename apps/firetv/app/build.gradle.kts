plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// vYEAR.MONTH.PATCH from git on every build, or the release's -P values.
apply(from = rootProject.file("../sand-version.gradle.kts"))
val sandVersionName = extra["sandVersionName"] as String
val sandVersionCode = extra["sandVersionCode"] as Int

android {
    namespace = "com.sandvault.tv"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.sandvault.tv"
        // Fire OS 5 (the first Fire TV Stick) is API 22; everything since is newer.
        minSdk = 22
        targetSdk = 34
        // The vault's own version as the name, and its commit count — which
        // only ever grows — as the code, so each build installs over the
        // last. Worked out from git on every build, or taken from the release
        // workflow's -P values; see apps/sand-version.gradle.kts.
        versionCode = sandVersionCode
        versionName = sandVersionName

        // Fire TVs are ARM — 32-bit userspace on most sticks, 64-bit on the
        // newest. The FFmpeg decoder ships x86 builds too, for emulators,
        // which would only make the download bigger.
        ndk {
            abiFilters += listOf("armeabi-v7a", "arm64-v8a")
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // Signed with the release key when the build is handed one (the
            // release workflow decodes it from the ANDROID_KEYSTORE_* secrets),
            // so every release installs over the last. Without one, the debug
            // key: it sideloads, but it differs from machine to machine, so an
            // APK signed with it never installs over one built elsewhere.
            signingConfig = System.getenv("ANDROID_KEYSTORE")?.takeIf { it.isNotEmpty() }?.let { path ->
                signingConfigs.create("release") {
                    storeFile = file(path)
                    storePassword = System.getenv("ANDROID_KEYSTORE_PASSWORD")
                    keyAlias = System.getenv("ANDROID_KEY_ALIAS")?.takeIf { it.isNotEmpty() } ?: "harbor"
                    keyPassword = System.getenv("ANDROID_KEY_PASSWORD")?.takeIf { it.isNotEmpty() }
                        ?: System.getenv("ANDROID_KEYSTORE_PASSWORD")
                }
            } ?: signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    testOptions {
        // The container tests run Media3's extractors on the JVM, where the
        // Android classes they touch in passing (SparseArray, Build) are
        // stubs; answering defaults is enough for sniffing bytes.
        unitTests.isReturnDefaultValues = true
    }
    lint {
        // A TV app has no touchscreen and no portrait mode; these checks are
        // about phones.
        disable += setOf("MissingLeanbackLauncher", "ImpliedTouchscreenHardware")
    }
}

dependencies {
    // Media3 and the FFmpeg decoder move together: Jellyfin builds the
    // decoder against one Media3 release, and the two must match.
    val media3 = "1.7.1"
    implementation("androidx.media3:media3-exoplayer:$media3")
    implementation("androidx.media3:media3-ui:$media3")
    // Audio the TV cannot decode itself, decoded in software by FFmpeg.
    // Jellyfin's build enables exactly: flac alac pcm_mulaw pcm_alaw mp3 aac
    // ac3 eac3 dca mlp truehd — so DTS (core), TrueHD and AC-3/E-AC-3 play on
    // a stick without those decoders and with no receiver to pass them to.
    // Media3 tries the TV's own decoders and HDMI passthrough first; FFmpeg
    // is the fallback. Audio only: its video renderer is an unfinished stub,
    // so video is the TV's hardware decoders' (see PlayerActivity).
    implementation("org.jellyfin.media3:media3-ffmpeg-decoder:1.6.1+2")
    implementation("androidx.annotation:annotation:1.8.2")

    testImplementation("junit:junit:4.13.2")
}
