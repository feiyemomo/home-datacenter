/*
 * HomeDatacenterClient.kt
 * ------------------------------------------------------------------
 * Phase 3 — Real-time communication reference client for Android.
 *
 * Demonstrates:
 *   1. Obtaining a 365-day JWT via Retrofit   (POST /api/v1/auth/bind)
 *   2. Connecting to the WebSocket             (OkHttp WebSocket)
 *   3. Sending an application-level heartbeat every 30 seconds
 *   4. Subscribing / unsubscribing to device topics
 *   5. Receiving and parsing the WsMessage envelope
 *   6. Reconnecting with exponential backoff
 *
 * Wire format (server <-> client JSON envelope):
 *   {"type":"...","topic":"...","payload":{...},"ts":1234567890}
 *
 * ------------------------------------------------------------------
 * Required build.gradle.kts (module: app) dependencies:
 *
 *   plugins {
 *       id("org.jetbrains.kotlin.plugin.serialization") version "<kotlin-ver>"
 *   }
 *
 *   dependencies {
 *       // Retrofit + OkHttp
 *       implementation("com.squareup.retrofit2:retrofit:2.11.0")
 *       implementation("com.squareup.okhttp3:okhttp:4.12.0")
 *       implementation("com.squareup.okhttp3:logging-interceptor:4.12.0")
 *
 *       // Kotlinx serialization + Retrofit converter
 *       implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.6.3")
 *       implementation("com.jakewharton.retrofit:retrofit2-kotlinx-serialization-converter:1.0.0")
 *
 *       // Coroutines
 *       implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.0")
 *
 *       // Lifecycle (lifecycleScope in the Activity sample)
 *       implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.0")
 *   }
 * ------------------------------------------------------------------
 */

package com.example.homecenter

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCallback
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.appcompat.app.AppCompatActivity
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.decodeFromJsonElement
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.logging.HttpLoggingInterceptor
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory
import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.Header
import retrofit2.http.POST
import retrofit2.http.Path
import java.util.concurrent.TimeUnit

private const val TAG = "HomeCenter"

// =====================================================================================
// 1. Data models
// =====================================================================================

/**
 * Unified API envelope returned by every /api/v1/* endpoint.
 *
 *   { "code": 0, "message": "success", "data": <T|null> }
 *
 * `code` mirrors the HTTP status (0 = success). The generic `data` field is
 * kept as [JsonElement] so each call can decode it into the concrete type it
 * expects, avoiding kotlinx.serialization generic-erasure pitfalls.
 */
@Serializable
data class ApiResponse(
    val code: Int = 0,
    val message: String = "",
    val data: JsonElement? = null
) {
    val isSuccess: Boolean get() = code == 0

    /** Decode the `data` payload, throwing on business-level errors. */
    fun <T> decodeOrNull(deserializer: kotlinx.serialization.KSerializer<T>): T? {
        require(isSuccess) { "API error $code: $message" }
        return data?.let { Json.decodeFromJsonElement(deserializer, it) }
    }

    /** Decode `data`, requiring a non-null payload. */
    fun <T> decode(deserializer: kotlinx.serialization.KSerializer<T>): T =
        decodeOrNull(deserializer) ?: error("API success but data was null")
}

/** Thrown when the API returns a non-zero business code. */
class ApiException(val code: Int, message: String) : RuntimeException("API $code: $message")

@Serializable
data class BindRequest(
    @SerialName("user_id") val userId: Long,
    @SerialName("access_key") val accessKey: String
)

@Serializable
data class BindData(
    val token: String
)

@Serializable
data class User(
    val id: Long,
    val name: String,
    @SerialName("is_admin") val isAdmin: Boolean
)

@Serializable
data class Device(
    val id: Long,
    @SerialName("user_id") val userId: Long,
    @SerialName("device_name") val deviceName: String,
    @SerialName("last_login_at") val lastLoginAt: String? = null,
    @SerialName("revoked_at") val revokedAt: String? = null,
    @SerialName("last_ip") val lastIp: String = "",
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("updated_at") val updatedAt: String = ""
)

@Serializable
data class DeviceList(
    @SerialName("devices") val devices: List<Device> = emptyList()
)

/**
 * Canonical WebSocket message envelope.
 *
 *   type    "heartbeat" | "event" | "subscribe" | "unsubscribe" | "broadcast" | "online_list" | "error"
 *   topic   event topic on server->client events; subscription prefix on subscribe/unsubscribe
 *   payload opaque JSON object (event-specific)
 *   ts      unix seconds
 */
@Serializable
data class WsMessage(
    val type: String,      // "heartbeat", "event", "subscribe", etc.
    val topic: String? = null,
    val payload: JsonObject? = null,
    val ts: Long = 0
)

/**
 * Client-side error report posted to /api/v1/system/client-errors.
 *
 * Field shapes mirror the backend SystemLog "client.error" writer (see
 * services/api/internal/handler/client_error_handler.go): the backend
 * rate-limits per user/IP and dedups on (context + message), so callers
 * SHOULD NOT spam — but a light client-side limiter/dedup is still
 * applied in [ClientErrorReporter] to be safe.
 *
 *   message  short human-readable description (server caps at 500)
 *   stack    optional stack trace (server caps at 2000)
 *   url      app context identifier (defaults to "android")
 *   level    "critical" | "normal" | "info"
 *   context  free-form tag for categorization, e.g. "android.ws"
 *   count    dedup counter (server merges identical reports)
 */
@Serializable
data class ClientErrorReport(
    val message: String,
    val stack: String = "",
    val url: String = "android",
    val level: String = "normal",
    val context: String = "android",
    val count: Int = 1
)

// =====================================================================================
// 2. REST API (Retrofit)
// =====================================================================================

/**
 * Retrofit interface for the Home Datacenter REST API.
 * All endpoints are suspend functions for coroutine-friendly calls.
 */
interface HomeCenterApi {

    /** POST /api/v1/auth/bind — exchange AccessKey for a 365-day JWT. */
    @POST("api/v1/auth/bind")
    suspend fun bindDevice(@Body req: BindRequest): ApiResponse

    /** GET /api/v1/user/me — current user profile. */
    @GET("api/v1/user/me")
    suspend fun getMe(@Header("Authorization") auth: String): ApiResponse

    /** GET /api/v1/device/list — devices visible to the caller. */
    @GET("api/v1/device/list")
    suspend fun listDevices(@Header("Authorization") auth: String): ApiResponse

    /** DELETE /api/v1/device/{id} — revoke a device (idempotent). */
    @DELETE("api/v1/device/{id}")
    suspend fun revokeDevice(
        @Header("Authorization") auth: String,
        @Path("id") id: Long
    ): ApiResponse

    /**
     * POST /api/v1/system/client-errors — server-side error reporting.
     * Backend persists the row as a SystemLog "client.error" entry with
     * per-user/IP rate limiting + dedup, so the app's own failures show up
     * in the dashboard log pane. (v1.8.42: Android client-side reporting.)
     */
    @POST("api/v1/system/client-errors")
    suspend fun reportClientError(
        @Header("Authorization") auth: String,
        @Body report: ClientErrorReport
    ): ApiResponse
}

// =====================================================================================
// 3. Repository — thin coroutine-friendly wrapper around the API
// =====================================================================================

/**
 * Repository that owns the Retrofit API and exposes typed, error-checked
 * suspend functions. Callers run these on Dispatchers.IO (or via
 * lifecycleScope which defaults to Main — wrap network calls with
 * withContext(Dispatchers.IO) when needed).
 */
class HomeCenterRepository(private val api: HomeCenterApi) {

    /**
     * Exchange (userId, accessKey) for a JWT.
     * @return the raw JWT string
     * @throws ApiException on a non-zero business code
     * @throws retrofit2.HttpException / IOException on transport errors
     */
    suspend fun bind(userId: Long, accessKey: String): String {
        val resp = api.bindDevice(BindRequest(userId, accessKey))
        ensureSuccess(resp)
        return resp.decode(BindData.serializer()).token
    }

    suspend fun getMe(token: String): User {
        val resp = api.getMe(bearer(token))
        ensureSuccess(resp)
        return resp.decode(User.serializer())
    }

    suspend fun listDevices(token: String): List<Device> {
        val resp = api.listDevices(bearer(token))
        ensureSuccess(resp)
        return resp.decode(DeviceList.serializer()).devices
    }

    suspend fun revokeDevice(token: String, deviceId: Long) {
        val resp = api.revokeDevice(bearer(token), deviceId)
        ensureSuccess(resp)
        // data is null on success — nothing to decode.
    }

    /**
     * Report a client-side error to the server. Best-effort: never throws.
     * The backend applies its own rate limiting + dedup, so repeated
     * identical reports collapse into one SystemLog row. (v1.8.42)
     */
    suspend fun reportError(token: String, report: ClientErrorReport) {
        try {
            api.reportClientError(bearer(token), report)
        } catch (t: Throwable) {
            // Reporting must never break the app or the WS reconnect loop.
            Log.w(TAG, "client error report failed: ${t.message}")
        }
    }

    private fun bearer(token: String): String = "Bearer $token"

    private fun ensureSuccess(resp: ApiResponse) {
        if (!resp.isSuccess) throw ApiException(resp.code, resp.message)
    }
}

// =====================================================================================
// 4. Factory — Retrofit + OkHttpClient builders
// =====================================================================================

object HomeCenterFactory {

    /** Configure kotlinx.serialization to tolerate server-side schema drift. */
    val json: Json = Json {
        ignoreUnknownKeys = true
        encodeDefaults = true
        explicitNulls = false
    }

    /**
     * Build a shared [OkHttpClient]. The same instance is reused for both
     * Retrofit and the WebSocket so connection pooling and timeouts are
     * consistent. Set [enableVerboseLogging] = false in production to avoid
     * logging tokens.
     */
    fun okHttpClient(enableVerboseLogging: Boolean = false): OkHttpClient {
        val builder = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .writeTimeout(30, TimeUnit.SECONDS)
            // OkHttp answers server pings automatically; we also set a
            // protocol-level ping interval as a safety net.
            .pingInterval(30, TimeUnit.SECONDS)

        if (enableVerboseLogging) {
            builder.addInterceptor(
                HttpLoggingInterceptor().apply { level = HttpLoggingInterceptor.Level.BASIC }
            )
        }
        return builder.build()
    }

    /** Build the Retrofit-backed [HomeCenterApi] for a given base URL. */
    fun createApi(baseUrl: String, client: OkHttpClient): HomeCenterApi {
        val contentType = "application/json".toMediaType()
        return Retrofit.Builder()
            .baseUrl(baseUrl)
            .client(client)
            .addConverterFactory(json.asConverterFactory(contentType))
            .build()
            .create(HomeCenterApi::class.java)
    }
}

// =====================================================================================
// 4.5 Client error reporter — rate-limited, deduped, background-safe
// =====================================================================================

/**
 * Client-side error reporter (v1.8.42).
 *
 * Mirrors the web reporter (web/src/lib/errorReport.ts): a thin, defensive,
 * fire-and-forget sender that pushes app/WS failures to
 * /api/v1/system/client-errors. The backend already rate-limits per
 * user/IP and dedups on (context+message), so this comparator only ADDS:
 *   - a global minimum interval (2s) so a reconnect storm can't flood,
 *   - a 60s in-memory dedup window that folds identical reports into one
 *     with an incremented `count` (belt-and-braces before the server),
 *   - coroutine dispatch on a caller-provided scope (reporting never
 *     throws and never blocks the caller).
 *
 * Usage from an Activity/Service:
 *   val reporter = ClientErrorReporter(token, repo, scope)
 *   reporter.report("android.ws", "ws failed: ${t.message}", level = "critical", stack = t.stackTraceToString())
 */
class ClientErrorReporter(
    private val token: String,
    private val repo: HomeCenterRepository,
    private val scope: CoroutineScope
) {
    private val MIN_INTERVAL_MS = 2_000L
    private val DEDUP_WINDOW_MS = 60_000L

    @Volatile private var lastSentAt = 0L
    private val recent = HashMap<String, MutableList<Long>>() // key -> send timestamps

    /**
     * Report an error. Never throws. [context] should be a stable tag
     * (e.g. "android.ws", "android.render") so the backend can group.
     * [stack] is truncated to 2000 chars to match the server cap.
     */
    fun report(
        context: String,
        message: String,
        level: String = "normal",
        stack: String? = null
    ) {
        val now = System.currentTimeMillis()
        synchronized(this) {
            if (now - lastSentAt < MIN_INTERVAL_MS) return
            lastSentAt = now
        }

        val key = "$context|$message"
        val keyTs = synchronized(recent) {
            val list = recent[key]
            if (list != null) recent[key] = list.filter { now - it < DEDUP_WINDOW_MS }
            when {
                list == null -> { recent[key] = mutableListOf(now); 1 }
                else -> { list.add(now); list.size }
            }
        }

        scope.launch {
            repo.reportError(
                token,
                ClientErrorReport(
                    message = message.take(500),
                    stack = stack?.take(2000) ?: "",
                    url = "android",
                    level = level,
                    context = context.take(64),
                    count = keyTs
                )
            )
        }
    }
}

// =====================================================================================
// 5. WebSocket client (OkHttp + coroutines)
// =====================================================================================

/**
 * Callbacks delivered on the OkHttp dispatcher thread. Switch to the UI
 * thread (or a coroutine context) before touching Android views.
 */
interface WsEventListener {
    /** Called after the WebSocket handshake succeeds and the heartbeat loop starts. */
    fun onConnected()

    /** Called for every inbound application message. */
    fun onMessage(message: WsMessage)

    /** Called when the connection has closed (a reconnect will be scheduled unless [disconnect] was called). */
    fun onDisconnected(code: Int, reason: String?)

    /** Called on transport failure; [reconnectAttempt] is the upcoming attempt number (1-based). */
    fun onError(throwable: Throwable, reconnectAttempt: Int)
}

/**
 * Production WebSocket client for the Home Datacenter real-time channel.
 *
 * Responsibilities:
 *   - Open the connection to `ws(s)://<host>/api/v1/ws` with the JWT.
 *   - Send an application-level `{"type":"heartbeat"}` every 30 s.
 *   - Subscribe / unsubscribe to topic prefixes (e.g. "device.1").
 *   - Parse inbound [WsMessage]s and dispatch them via [WsEventListener].
 *   - Reconnect with exponential backoff on [onClosed] / [onFailure].
 *
 * Thread-safety: the underlying [WebSocket] is safe to call from any thread.
 * Mutations of [webSocket] / [isConnected] are guarded with @Volatile.
 *
 * @param wsUrl absolute WebSocket URL, e.g. "ws://192.168.1.10:8080/api/v1/ws"
 * @param token the JWT obtained from /api/v1/auth/bind
 * @param scope coroutine scope used for the heartbeat and reconnect loops;
 *              cancelling it stops both loops. Typically a SupervisorJob-backed
 *              scope tied to the Activity / Fragment / Service lifecycle.
 */
class HomeCenterWebSocket(
    private val client: OkHttpClient,
    private val wsUrl: String,
    private val token: String,
    private val listener: WsEventListener,
    private val scope: CoroutineScope,
    private val heartbeatIntervalMs: Long = 30_000L,
    /**
     * Optional callback invoked on transport failure ([onFailure]) BEFORE a
     * reconnect is scheduled. Wire this to a [ClientErrorReporter] so WS
     * failures surface in the dashboard log pane (v1.8.42).
     */
    private val onErrorReport: ((Throwable) -> Unit)? = null
) {

    @Volatile private var webSocket: WebSocket? = null
    @Volatile private var isConnected: Boolean = false
    @Volatile private var shouldReconnect: Boolean = true

    private var heartbeatJob: Job? = null
    private var reconnectJob: Job? = null
    private var reconnectAttempt: Int = 0

    /** Topics that should be re-applied after every reconnect. */
    private val activeSubscriptions: MutableSet<String> = LinkedHashSet()

    /** Whether the socket is currently open. Thread-safe. */
    fun isConnected(): Boolean = isConnected

    /** Start the connection. Safe to call multiple times. */
    fun connect() {
        if (webSocket != null) return

        // Auth: the server accepts either the Authorization: Bearer header
        // (preferred — keeps the token out of URL logs) or ?token= query param
        // (used by browsers). We use the header here.
        val request = Request.Builder()
            .url(wsUrl)
            .header("Authorization", "Bearer $token")
            .build()

        webSocket = client.newWebSocket(request, WsListener())
    }

    /**
     * Called when connectivity returns (e.g. WiFi↔cellular switch detected
     * by a ConnectivityManager NetworkCallback). If the socket isn't
     * connected, cancel any pending backoff timer and reconnect immediately
     * instead of waiting out the remaining delay. (v1.8.42)
     */
    fun onNetworkAvailable() {
        if (isConnected) return
        reconnectJob?.cancel()
        reconnectJob = null
        if (shouldReconnect) {
            Log.i(TAG, "network available — reconnecting immediately")
            webSocket = null
            connect()
        }
    }

    /** Gracefully close and stop auto-reconnect. Idempotent. */
    fun disconnect() {
        shouldReconnect = false
        cancelLoops()
        webSocket?.close(NORMAL_CLOSURE, "client disconnect")
        webSocket = null
        isConnected = false
    }

    /** Send a raw [WsMessage]; returns false if the socket is not open. */
    fun send(message: WsMessage): Boolean {
        val ws = webSocket ?: return false
        val text = HomeCenterFactory.json.encodeToString(WsMessage.serializer(), message)
        return ws.send(text)
    }

    /** Subscribe to a topic prefix (e.g. "device.1") and remember it for reconnects. */
    fun subscribe(topic: String) {
        synchronized(activeSubscriptions) { activeSubscriptions.add(topic) }
        send(WsMessage(type = TYPE_SUBSCRIBE, topic = topic))
    }

    /** Stop receiving events for a topic prefix. */
    fun unsubscribe(topic: String) {
        synchronized(activeSubscriptions) { activeSubscriptions.remove(topic) }
        send(WsMessage(type = TYPE_UNSUBSCRIBE, topic = topic))
    }

    /** Send a single application-level heartbeat. */
    fun sendHeartbeat(): Boolean = send(WsMessage(type = TYPE_HEARTBEAT))

    // ---- internals -----------------------------------------------------------------

    private fun startHeartbeat() {
        heartbeatJob?.cancel()
        heartbeatJob = scope.launch {
            while (isActive && isConnected) {
                delay(heartbeatIntervalMs)
                if (!sendHeartbeat()) {
                    Log.w(TAG, "heartbeat send failed; connection likely dead")
                    break
                }
            }
        }
    }

    private fun stopHeartbeat() {
        heartbeatJob?.cancel()
        heartbeatJob = null
    }

    private fun cancelLoops() {
        stopHeartbeat()
        reconnectJob?.cancel()
        reconnectJob = null
    }

    /** Schedule a reconnect with exponential backoff: 1s, 2s, 4s ... capped at 30s. */
    private fun scheduleReconnect() {
        if (!shouldReconnect) return
        if (reconnectJob?.isActive == true) return // already pending

        reconnectAttempt += 1
        val delayMs = (1L shl (reconnectAttempt - 1).coerceAtMost(5)) * 1000L // 1..32s
        val cappedDelay = delayMs.coerceAtMost(30_000L)

        Log.i(TAG, "scheduling reconnect #$reconnectAttempt in $cappedDelay ms")
        reconnectJob = scope.launch {
            delay(cappedDelay)
            if (shouldReconnect) {
                // Tear down the previous socket before opening a new one.
                webSocket = null
                isConnected = false
                connect()
            }
        }
    }

    private fun handleInbound(text: String) {
        val msg: WsMessage = try {
            HomeCenterFactory.json.decodeFromString(WsMessage.serializer(), text)
        } catch (t: Throwable) {
            Log.w(TAG, "failed to parse inbound frame: ${t.message}")
            return
        }
        listener.onMessage(msg)
    }

    private inner class WsListener : WebSocketListener() {

        override fun onOpen(webSocket: WebSocket, response: okhttp3.Response) {
            Log.i(TAG, "ws connected (http=${response.code})")
            isConnected = true
            reconnectAttempt = 0
            this@HomeCenterWebSocket.webSocket = webSocket

            // Re-apply subscriptions so the client keeps receiving the same
            // topics across reconnects.
            val subs = synchronized(activeSubscriptions) { activeSubscriptions.toList() }
            subs.forEach { send(WsMessage(type = TYPE_SUBSCRIBE, topic = it)) }

            startHeartbeat()
            listener.onConnected()
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            handleInbound(text)
        }

        override fun onMessage(webSocket: WebSocket, bytes: okio.ByteString) {
            // The server only sends text frames; ignore binary.
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            // Acknowledge the close frame so the peer can tear down cleanly.
            webSocket.close(code, reason)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            Log.i(TAG, "ws closed: $code $reason")
            isConnected = false
            stopHeartbeat()
            listener.onDisconnected(code, reason)
            // 1000 (normal) and going-away are still recoverable for a long-lived
            // app session; schedule a reconnect unless the user called disconnect().
            if (shouldReconnect) scheduleReconnect()
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: okhttp3.Response?) {
            Log.w(TAG, "ws failure: ${t.message}")
            isConnected = false
            stopHeartbeat()
            this@HomeCenterWebSocket.webSocket = null
            listener.onError(t, reconnectAttempt + 1)
            // v1.8.42: surface transport failures to the backend before the
            // reconnect loop buries them under retries.
            onErrorReport?.invoke(t)
            if (shouldReconnect) scheduleReconnect()
        }
    }

    companion object {
        private const val TYPE_HEARTBEAT = "heartbeat"
        private const val TYPE_SUBSCRIBE = "subscribe"
        private const val TYPE_UNSUBSCRIBE = "unsubscribe"

        // 1000 = normal closure. Sent by the client in disconnect().
        private const val NORMAL_CLOSURE = 1000
    }
}

// =====================================================================================
// 5.5 Network monitor — reconnect on connectivity changes
// =====================================================================================

/**
 * Wraps [ConnectivityManager] to observe network availability and surface
 * connectivity recovery to the WebSocket. (v1.8.42)
 *
 * WiFi↔cellular handoffs and airplane-mode toggles tear down the current
 * socket but don't always fire OkHttp's onFailure promptly enough; hopping
 * on [onAvailable] lets the WS reconnect immediately instead of waiting out
 * its backoff timer. Requires android.permission.ACCESS_NETWORK_STATE.
 */
class NetworkMonitor(
    context: Context,
    private val onAvailable: () -> Unit
) : NetworkCallback() {

    private val connectivityManager =
        context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
    private var registered = false

    /** Register the default-network callback. Safe to call once. */
    fun register() {
        if (registered) return
        registered = true
        connectivityManager.registerDefaultNetworkCallback(this)
    }

    /** Unregister. Safe to call once. */
    fun unregister() {
        if (!registered) return
        registered = false
        connectivityManager.unregisterNetworkCallback(this)
    }

    override fun onAvailable(network: Network) {
        onAvailable()
    }
}

// =====================================================================================
// 5.6 Foreground service — keep the WS alive in the background
// =====================================================================================

/**
 * Foreground service that hosts the WebSocket so the realtime channel
 * survives the app being backgrounded (Android can otherwise kill a
 * backgrounded process — the WS drops and the dashboard stops updating).
 * (v1.8.42)
 *
 * Start via `startForegroundService(intent)`. The service shows a
 * persistent notification; on API 34+ (U) it additionally requires the
 * FOREGROUND_SERVICE_CONNECTED_DEVICE or FOREGROUND_SERVICE_DATA_SYNC
 * foreground-service type declared in the manifest.
 *
 * Required manifest additions:
 *   <uses-permission android:name="android.permission.INTERNET"/>
 *   <uses-permission android:name="android.permission.ACCESS_NETWORK_STATE"/>
 *   <uses-permission android:name="android.permission.FOREGROUND_SERVICE"/>
 *   <uses-permission android:name="android.permission.POST_NOTIFICATIONS"/>
 *   <uses-permission
 *       android:name="android.permission.FOREGROUND_SERVICE_CONNECTED_DEVICE"/>
 *   <service android:name=".HomeCenterService"
 *            android:foregroundServiceType="connectedDevice"
 *            android:exported="false"/>
 *
 * The WebSocket is created lazily on [onCreate] and torn down on [onDestroy];
 * the [NetworkMonitor] triggers an immediate reconnect on connectivity
 * recovery. Error reporting is wired via [ClientErrorReporter] so service-
 * level WS failures still reach the dashboard log pane.
 */
class HomeCenterService : Service() {

    private val ioScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var okHttp: OkHttpClient? = null
    private var webSocket: HomeCenterWebSocket? = null
    private var networkMonitor: NetworkMonitor? = null
    private var errorReporter: ClientErrorReporter? = null

    override fun onCreate() {
        super.onCreate()
        startAsForeground()
        startRealtime()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // START_STICKY: if the system kills the service, restart it with a
        // null intent (and an empty command) so the WS comes back up.
        return START_STICKY
    }

    override fun onDestroy() {
        networkMonitor?.unregister()
        networkMonitor = null
        webSocket?.disconnect()
        webSocket = null
        ioScope.cancel()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun startAsForeground() {
        val channel = NotificationChannel(
            CHANNEL_ID,
            "Home Datacenter",
            NotificationManager.IMPORTANCE_LOW
        ).apply { setShowBadge(false) }
        getSystemService(NotificationManager::class.java).createNotificationChannel(channel)

        val notification: Notification = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("Home Datacenter")
            .setContentText("实时连接保持中")
            .setSmallIcon(android.R.drawable.ic_menu_compass)
            .setOngoing(true)
            .build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        } else {
            startForeground(NOTIFICATION_ID, notification, 0)
        }
    }

    private fun startRealtime() {
        // Base URL / WS URL must come from real config (constants here are
        // placeholders mirroring the Activity sample).
        val baseUrl = "http://10.0.2.2:8080/"
        val wsUrl = "ws://10.0.2.2:8080/api/v1/ws"

        okHttp = HomeCenterFactory.okHttpClient(enableVerboseLogging = false)
        val client = okHttp ?: return
        val api = HomeCenterFactory.createApi(baseUrl, client)
        val repo = HomeCenterRepository(api)

        // NOTE: persist the token in EncryptedSharedPreferences and load it
        // here instead of re-binding on every service start.
        val token = "LOAD_FROM_STORAGE"

        errorReporter = ClientErrorReporter(token, repo, ioScope)

        val ws = HomeCenterWebSocket(
            client = client,
            wsUrl = wsUrl,
            token = token,
            scope = ioScope,
            listener = object : WsEventListener {
                override fun onConnected() { /* re-subscribe to topics here */ }
                override fun onMessage(message: WsMessage) { /* dispatch */ }
                override fun onDisconnected(code: Int, reason: String?) { /* optional */ }
                override fun onError(throwable: Throwable, reconnectAttempt: Int) {
                    errorReporter?.report(
                        context = "android.ws",
                        message = "service ws error: ${throwable.message}",
                        level = "critical",
                        stack = throwable.stackTraceToString()
                    )
                }
            },
            onErrorReport = { t ->
                errorReporter?.report(
                    context = "android.ws",
                    message = "service ws failure: ${t.message}",
                    level = "critical",
                    stack = t.stackTraceToString()
                )
            }
        )
        webSocket = ws
        ws.connect()

        networkMonitor = NetworkMonitor(this) { ws.onNetworkAvailable() }
        networkMonitor?.register()
    }

    companion object {
        private const val CHANNEL_ID = "home_datacenter"
        private const val NOTIFICATION_ID = 1001
    }
}

// =====================================================================================
// 6. Sample Activity — end-to-end usage
// =====================================================================================

/**
 * Minimal Activity wiring everything together:
 *   - builds Retrofit + OkHttp,
 *   - binds the device to obtain a JWT,
 *   - opens the WebSocket and subscribes to "device.1",
 *   - prints inbound events to logcat.
 *
 * In a real app you would inject these (Hilt/Koin) and persist the token
 * in EncryptedSharedPreferences instead of re-binding every launch.
 *
 * NOTE: requires INTERNET permission in AndroidManifest.xml.
 */
class HomeCenterActivity : AppCompatActivity() {

    private lateinit var okHttp: OkHttpClient
    private lateinit var api: HomeCenterApi
    private lateinit var repo: HomeCenterRepository

    private var webSocket: HomeCenterWebSocket? = null

    /** Supervisor-backed scope so a single WS failure doesn't cancel siblings. */
    private val ioScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onCreate(savedInstanceState: android.os.Bundle?) {
        super.onCreate(savedInstanceState)

        val baseUrl = "http://10.0.2.2:8080/"        // Android emulator -> host loopback
        val wsUrl = "ws://10.0.2.2:8080/api/v1/ws"

        okHttp = HomeCenterFactory.okHttpClient(enableVerboseLogging = true)
        api = HomeCenterFactory.createApi(baseUrl, okHttp)
        repo = HomeCenterRepository(api)

        // Drive the bind + WS lifecycle off the Activity's lifecycleScope.
        lifecycleScope.launch {
            try {
                // 1. Obtain the JWT. Replace with your real user_id + AccessKey.
                val token = repo.bind(userId = 1L, accessKey = ACCESS_KEY)
                Log.i(TAG, "bind ok, token length=${token.length}")

                // 2. Sanity check: fetch the current user.
                val me = repo.getMe(token)
                Log.i(TAG, "user: id=${me.id} name=${me.name} admin=${me.isAdmin}")

                // 3. Open the WebSocket and subscribe.
                val ws = HomeCenterWebSocket(
                    client = okHttp,
                    wsUrl = wsUrl,
                    token = token,
                    scope = ioScope,
                    listener = object : WsEventListener {
                        override fun onConnected() {
                            Log.i(TAG, "ws onConnected")
                            // Subscribe to every event for device 1.
                            // (Prefix match: device.1 catches device.1.status, etc.)
                            webSocket?.subscribe("device.1")
                        }

                        override fun onMessage(message: WsMessage) {
                            // Route by type. `payload` is a JsonObject — decode it
                            // into a concrete type when you know the topic.
                            when (message.type) {
                                "heartbeat" ->
                                    Log.d(TAG, "heartbeat ack: ${message.payload}")
                                "event" ->
                                    Log.i(TAG, "event topic=${message.topic} payload=${message.payload}")
                                "online_list" ->
                                    Log.i(TAG, "online devices: ${message.payload}")
                                "broadcast" ->
                                    Log.i(TAG, "broadcast: ${message.payload}")
                                "error" ->
                                    Log.w(TAG, "server error: ${message.payload}")
                                else ->
                                    Log.d(TAG, "msg type=${message.type} topic=${message.topic}")
                            }
                        }

                        override fun onDisconnected(code: Int, reason: String?) {
                            Log.i(TAG, "ws disconnected code=$code reason=$reason")
                        }

                        override fun onError(throwable: Throwable, reconnectAttempt: Int) {
                            Log.w(TAG, "ws error (attempt=$reconnectAttempt): ${throwable.message}")
                        }
                    }
                )
                webSocket = ws
                ws.connect()

            } catch (t: Throwable) {
                // ApiException, retrofit2.HttpException, IOException, etc.
                Log.e(TAG, "startup failed: ${t.javaClass.simpleName}: ${t.message}", t)
            }
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        // Stop the WebSocket and its heartbeat / reconnect loops.
        webSocket?.disconnect()
        webSocket = null
        ioScope.cancel()
    }

    private companion object {
        // Replace with a real 64-char hex AccessKey from `scripts/create_device.go`.
        const val ACCESS_KEY = "e6b9b928fc277d062943a46942c07d85b6a99ef4c4d5bc74d737c9cfd1ff304a"
    }
}
