plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.example.homecenter"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.example.homecenter"
        minSdk = 24
        targetSdk = 36
        versionCode = 124
        versionName = "1.8.43"

        // Build-time config baked into BuildConfig for easy wiring.
        buildConfigField("String", "DEFAULT_BASE_URL", "\"http://192.168.31.235:8080/\"")
        buildConfigField("String", "DEFAULT_WS_URL", "\"wss://nas.feiyemomo.top/api/v1/ws\"")
    }

    buildTypes {
        release {
            // Production: replace with a real release keystore via the
            // signingConfigs block below. For local builds we fall back to the
            // debug key so `gradlew assembleRelease` still succeeds.
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            signingConfig = signingConfigs.getByName("debug")
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
}