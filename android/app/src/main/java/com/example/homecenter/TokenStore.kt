package com.example.homecenter

import android.content.Context
import android.content.SharedPreferences

/**
 * Persists the JWT and connection config so the foreground service can start
 * without re-binding on every launch.
 *
 * NOTE: plain SharedPreferences keeps the token readable on a rooted device.
 * For hardened deployments, swap the backing store for an
 * EncryptedSharedPreferences instance (androidx.security:security-crypto)
 * keyed by the app's master key — the call sites below already go through
 * this class, so the swap is a one-line change here only.
 */
class TokenStore(context: Context) {

    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    var token: String
        get() = prefs.getString(KEY_TOKEN, "") ?: ""
        set(value) = prefs.edit().putString(KEY_TOKEN, value).apply()

    var userId: Long
        get() = prefs.getLong(KEY_USER_ID, 0L)
        set(value) = prefs.edit().putLong(KEY_USER_ID, value).apply()

    var baseUrl: String
        get() = prefs.getString(KEY_BASE_URL, BuildConfig.DEFAULT_BASE_URL) ?: BuildConfig.DEFAULT_BASE_URL
        set(value) = prefs.edit().putString(KEY_BASE_URL, value).apply()

    var wsUrl: String
        get() = prefs.getString(KEY_WS_URL, BuildConfig.DEFAULT_WS_URL) ?: BuildConfig.DEFAULT_WS_URL
        set(value) = prefs.edit().putString(KEY_WS_URL, value).apply()

    fun clear() {
        prefs.edit().clear().apply()
    }

    /**
     * The controller returns camera stream URLs as *relative* paths
     * (`/go2rtc/api/stream.m3u8?...`). Resolve them against [baseUrl] (which
     * must point at the web-nginx origin that fronts both /api/ and /go2rtc/)
     * to get an absolute URL ExoPlayer / OkHttp can open.
     */
    fun resolveAbsolute(raw: String): String {
        if (raw.isEmpty()) return ""
        if (raw.startsWith("http://") || raw.startsWith("https://")) return raw
        val base = baseUrl.trimEnd('/')
        return base + (if (raw.startsWith("/")) raw else "/$raw")
    }

    companion object {
        private const val PREFS_NAME = "home_datacenter"
        private const val KEY_TOKEN = "jwt"
        private const val KEY_USER_ID = "user_id"
        private const val KEY_BASE_URL = "base_url"
        private const val KEY_WS_URL = "ws_url"
    }
}