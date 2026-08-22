plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.serialization")
}

import java.util.Properties

// Load release signing credentials from app/keystore.properties (gitignored).
// Falls back to the debug key so local debug builds still succeed when the
// properties file is absent.
val keystoreProps = Properties().apply {
    val f = rootProject.file("app/keystore.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}
fun prop(key: String, default: String = "") =
    keystoreProps.getProperty(key)?.takeIf { it.isNotBlank() } ?: default

android {
    namespace = "com.example.homecenter"
    compileSdk = 36

    signingConfigs {
        create("release") {
            storeFile = file(prop("RELEASE_STORE_FILE", "keystore/home-release.jks"))
            storePassword = prop("RELEASE_STORE_PASSWORD")
            keyAlias = prop("RELEASE_KEY_ALIAS", "home-release")
            keyPassword = prop("RELEASE_KEY_PASSWORD")
            enableV1Signing = true
            enableV2Signing = true
            enableV3Signing = true
        }
    }

    defaultConfig {
        applicationId = "com.example.homecenter"
        minSdk = 24
        targetSdk = 36
        versionCode = 125
        versionName = "1.8.47"

        // Build-time config baked into BuildConfig for easy wiring.
        // The base URL MUST point at the web-nginx origin (not home-api:8080):
        // it fronts BOTH /api/** (home-api) and /go2rtc/** (Frigate's go2rtc,
        // JWT auth_request gate); HLS live is only reachable through it.
        buildConfigField("String", "DEFAULT_BASE_URL", "\"http://192.168.31.235:8088/\"")
        buildConfigField("String", "DEFAULT_WS_URL", "\"ws://192.168.31.235:8088/api/v1/ws\"")
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Official release keystore (home-release.jks). If keystore.properties
            // is absent we fall back to the debug key so assembleRelease still runs.
            val cfgName = if (prop("RELEASE_STORE_PASSWORD").isNotEmpty()) "release" else "debug"
            signingConfig = signingConfigs.getByName(cfgName)
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        buildConfig = true
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")

    // Coroutines
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")

    // Kotlinx serialization + Retrofit converter
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")
    implementation("com.squareup.retrofit2:retrofit:2.11.0")
    implementation("com.jakewharton.retrofit:retrofit2-kotlinx-serialization-converter:1.0.0")

    // OkHttp
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    implementation("com.squareup.okhttp3:logging-interceptor:4.12.0")

    // Video playback — media3 (ExoPlayer): HLS live + progressive fMP4.
    implementation("androidx.media3:media3-exoplayer:1.4.1")
    implementation("androidx.media3:media3-exoplayer-hls:1.4.1")
    implementation("androidx.media3:media3-ui:1.4.1")
    implementation("androidx.media3:media3-common:1.4.1")
}