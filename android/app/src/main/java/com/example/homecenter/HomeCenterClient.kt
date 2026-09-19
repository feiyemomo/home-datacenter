/*
 * HomeCenterClient.kt
 * ------------------------------------------------------------------
 * Real-time communication core for the Home Datacenter Android app.
 *
 * Ported from deploy/android/HomeDatacenterClient.kt (the single-file
 * reference client) into a buildable Gradle module. This file keeps the
 * pure protocol layer — data models, Retrofit API, WebSocket client and
 * the resilience helpers — without any Android UI or Service wiring.
 *
 * Wire formats:
 *   REST envelope : { "code": 0, "message": "success", "data": <T|null> }
 *   WS envelope   : {"type":"...","topic":"...","payload":{...},"ts":123}
 */

package com.example.homecenter

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.ConnectivityManager.NetworkCallback
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.builtins.ListSerializer
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.logging.HttpLoggingInterceptor
import retrofit2.Retrofit
import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.Header
import retrofit2.http.POST
import retrofit2.http.Path
import retrofit2.http.Query
import java.util.concurrent.TimeUnit

private const val TAG = "HomeCenter"

// =====================================================================================
// 1. Data models
// =====================================================================================

/**
 * Unified API envelope returned by every /api/v1/ endpoint.
 * `data` is kept as [JsonElement] so each call decodes it into the concrete
 * type it expects, avoiding kotlinx.serialization generic-erasure pitfalls.
 */
@Serializable
data class ApiResponse(
    val code: Int = 0,
    val message: String = "",
    val data: JsonElement? = null
) {
    val isSuccess: Boolean get() = code == 0

    fun <T> decodeOrNull(deserializer: kotlinx.serialization.KSerializer<T>): T? {
        require(isSuccess) { "API error $code: $message" }
        return data?.let { Json.decodeFromJsonElement(deserializer, it) }
    }

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
data class BindData(val token: String)

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

// =====================================================================================
// 1.5 Camera / recording models (aligned with the web CameraStream / CameraRecording)
// =====================================================================================

/**
 * Live-stream URLs for a camera. The controller returns these as *relative*
 * paths (`/go2rtc/...`) because the deployment proxy (web nginx) fronts both
 * the /api path (home-api) and the /go2rtc path (Frigate's bundled go2rtc) on
 * one origin with a JWT `auth_request` gate. Resolve them against
 * [TokenStore.baseUrl] before playback.
 */
@Serializable
data class CameraStream(
    @SerialName("stream_name") val streamName: String = "",
    @SerialName("webrtc_url") val webrtcUrl: String = "",
    @SerialName("hls_url") val hlsUrl: String = "",
    // HLS passthrough of the camera's native-HEVC companion stream
    // (<name>_hevc). Empty for non-HEVC cameras.
    @SerialName("hls_hevc_url") val hlsHevcUrl: String = ""
)

@Serializable
data class Camera(
    val id: Long = 0,
    val name: String = "",
    val type: String = "",
    val status: String = "offline",   // online / offline / unknown
    val codec: String = "",            // h264 / hevc / ...
    val transcode: Boolean = false,
    val capabilities: JsonElement? = null,
    val stream: CameraStream? = null
) {
    val isOnline: Boolean get() = status == "online"
}

/** A 60-second recording segment. `id` is the minute-start unix timestamp. */
@Serializable
data class CameraRecording(
    val id: Long = 0,
    @SerialName("camera_id") val cameraId: Long = 0,
    @SerialName("start_at") val startAt: String = "",
    @SerialName("end_at") val endAt: String = "",
    @SerialName("duration_seconds") val durationSeconds: Long = 60,
    @SerialName("segment_count") val segmentCount: Int = 0
)

/**
 * Canonical WebSocket message envelope.
 *   type  "heartbeat" | "event" | "subscribe" | "unsubscribe" | "broadcast" | "online_list" | "error"
 */
@Serializable
data class WsMessage(
    val type: String,
    val topic: String? = null,
    val payload: JsonObject? = null,
    val ts: Long = 0
)

/**
 * Client-side error report posted to /api/v1/system/client-errors.
 * The backend rate-limits per user/IP and dedups on (context + message).
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

interface HomeCenterApi {

    /** POST /api/v1/auth/bind — exchange AccessKey for a 365-day JWT. */
    @POST("api/v1/auth/bind")
    suspend fun bindDevice(@Body req: BindRequest): ApiResponse

    /** POST /api/v1/auth/refresh — re-issue a fresh 365-day JWT for the caller. */
    @POST("api/v1/auth/refresh")
    suspend fun refreshToken(@Header("Authorization") auth: String): ApiResponse

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

    /** POST /api/v1/system/client-errors — server-side error reporting. */
    @POST("api/v1/system/client-errors")
    suspend fun reportClientError(
        @Header("Authorization") auth: String,
        @Body report: ClientErrorReport
    ): ApiResponse

    // ---- cameras & recordings (added: video-client upgrade) ------------------

    /** GET /api/v1/cameras — all cameras the caller may view. */
    @GET("api/v1/cameras")
    suspend fun listCameras(@Header("Authorization") auth: String): ApiResponse

    /**
     * GET /api/v1/cameras/{id}/recordings — recording segments, defaulting to
     * the last 7 days. Pass after/before (unix seconds) to page further back.
     */
    @GET("api/v1/cameras/{id}/recordings")
    suspend fun listRecordings(
        @Header("Authorization") auth: String,
        @Path("id") id: Long,
        @Query("after") after: Long? = null,
        @Query("before") before: Long? = null,
        @Query("limit") limit: Long? = null
    ): ApiResponse
}

// =====================================================================================
// 3. Repository
// =====================================================================================

class HomeCenterRepository(private val api: HomeCenterApi) {

    suspend fun bind(userId: Long, accessKey: String): String {
        val resp = api.bindDevice(BindRequest(userId, accessKey))
        ensureSuccess(resp)
        return resp.decode(BindData.serializer()).token
    }

    suspend fun refreshToken(token: String): String {
        val resp = api.refreshToken(bearer(token))
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
    }

    /** GET /api/v1/cameras → the cameras the caller may view. */
    suspend fun listCameras(token: String): List<Camera> {
        val resp = api.listCameras(bearer(token))
        ensureSuccess(resp)
        return resp.decode(ListSerializer(Camera.serializer()))
    }

    /**
     * GET /api/v1/cameras/{id}/recordings. Without after/before the server
     * returns the last 7 days; pass after/before (unix seconds) to page older.
     */
    suspend fun listRecordings(
        token: String,
        cameraId: Long,
        after: Long? = null,
        before: Long? = null,
        limit: Long? = null
    ): List<CameraRecording> {
        val resp = api.listRecordings(bearer(token), cameraId, after, before, limit)
        ensureSuccess(resp)
        return resp.decode(ListSerializer(CameraRecording.serializer()))
    }

    /** Report a client error. Best-effort: never throws. */
    suspend fun reportError(token: String, report: ClientErrorReport) {
        try {
            api.reportClientError(bearer(token), report)
        } catch (t: Throwable) {
            Log.w(TAG, "client error report failed: ${t.message}")
        }
    }

    private fun bearer(token: String): String = "Bearer $token"

    private fun ensureSuccess(resp: ApiResponse) {
        if (!resp.isSuccess) throw ApiException(resp.code, resp.message)
    }
}

// =====================================================================================
// 4. Factory
// =====================================================================================

object HomeCenterFactory {

    /** Tolerate server-side schema drift. */
    val json: Json = Json {
        ignoreUnknownKeys = true
        encodeDefaults = true
        explicitNulls = false
    }

    fun okHttpClient(enableVerboseLogging: Boolean = false): OkHttpClient {
        val builder = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .writeTimeout(30, TimeUnit.SECONDS)
            .pingInterval(30, TimeUnit.SECONDS)

        if (enableVerboseLogging) {
            builder.addInterceptor(
                HttpLoggingInterceptor().apply { level = HttpLoggingInterceptor.Level.BASIC }
            )
        }
        return builder.build()
    }

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
 * Rate-limited, deduped, fire-and-forget error reporter. Never throws.
 * The backend already limits per user/IP and dedups on (context+message);
 * this ADDS a 2s global minimum interval and a 60s in-memory dedup window.
 */
class ClientErrorReporter(
    private val token: String,
    private val repo: HomeCenterRepository,
    private val scope: CoroutineScope
) {
    private val MIN_INTERVAL_MS = 2_000L
    private val DEDUP_WINDOW_MS = 60_000L

    @Volatile private var lastSentAt = 0L
    private val recent = HashMap<String, MutableList<Long>>()

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
            if (list != null) recent[key] = list.filter { now - it < DEDUP_WINDOW_MS }.toMutableList()
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
// 5. WebSocket client
// =====================================================================================

interface WsEventListener {
    fun onConnected()
    fun onMessage(message: WsMessage)
    fun onDisconnected(code: Int, reason: String?)
    fun onError(throwable: Throwable, reconnectAttempt: Int)
}

/**
 * Production WebSocket client with heartbeat, subscription persistence and
 * exponential-backoff reconnect. Optionally reports transport failures to the
 * backend via [onErrorReport] before the reconnect loop buries them.
 */
class HomeCenterWebSocket(
    private val client: OkHttpClient,
    private val wsUrl: String,
    private val token: String,
    private val listener: WsEventListener,
    private val scope: CoroutineScope,
    private val heartbeatIntervalMs: Long = 30_000L,
    private val onErrorReport: ((Throwable) -> Unit)? = null
) {

    @Volatile private var webSocket: WebSocket? = null
    @Volatile private var isConnected: Boolean = false
    @Volatile private var shouldReconnect: Boolean = true

    private var heartbeatJob: Job? = null
    private var reconnectJob: Job? = null
    private var reconnectAttempt: Int = 0

    private val activeSubscriptions: MutableSet<String> = LinkedHashSet()

    fun isConnected(): Boolean = isConnected

    fun connect() {
        if (webSocket != null) return

        val request = Request.Builder()
            .url(wsUrl)
            .header("Authorization", "Bearer $token")
            // The deployment proxy (web nginx /api/v1/ws block) authenticates
            // the WS handshake via the Sec-WebSocket-Protocol subprotocol
            // (`bearer.<jwt>`), not the Authorization header (which nginx
            // discards on upgrade). Adding it is harmless on a direct LAN
            // connection where the backend reads the header instead.
            .header("Sec-WebSocket-Protocol", "bearer.$token")
            .build()

        webSocket = client.newWebSocket(request, WsListener())
    }

    /** Reconnect immediately when connectivity returns, skipping the backoff wait. */
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

    fun disconnect() {
        shouldReconnect = false
        cancelLoops()
        webSocket?.close(NORMAL_CLOSURE, "client disconnect")
        webSocket = null
        isConnected = false
    }

    fun send(message: WsMessage): Boolean {
        val ws = webSocket ?: return false
        val text = HomeCenterFactory.json.encodeToString(WsMessage.serializer(), message)
        return ws.send(text)
    }

    fun subscribe(topic: String) {
        synchronized(activeSubscriptions) { activeSubscriptions.add(topic) }
        send(WsMessage(type = TYPE_SUBSCRIBE, topic = topic))
    }

    fun unsubscribe(topic: String) {
        synchronized(activeSubscriptions) { activeSubscriptions.remove(topic) }
        send(WsMessage(type = TYPE_UNSUBSCRIBE, topic = topic))
    }

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

    /** Exponential backoff: 1s, 2s, 4s ... capped at 30s. */
    private fun scheduleReconnect() {
        if (!shouldReconnect) return
        if (reconnectJob?.isActive == true) return

        reconnectAttempt += 1
        val delayMs = (1L shl (reconnectAttempt - 1).coerceAtMost(5)) * 1000L
        val cappedDelay = delayMs.coerceAtMost(30_000L)

        Log.i(TAG, "scheduling reconnect #$reconnectAttempt in $cappedDelay ms")
        reconnectJob = scope.launch {
            delay(cappedDelay)
            if (shouldReconnect) {
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

            val subs = synchronized(activeSubscriptions) { activeSubscriptions.toList() }
            subs.forEach { send(WsMessage(type = TYPE_SUBSCRIBE, topic = it)) }

            startHeartbeat()
            listener.onConnected()
        }

        override fun onMessage(webSocket: WebSocket, text: String) = handleInbound(text)

        override fun onMessage(webSocket: WebSocket, bytes: okio.ByteString) {
            // Server only sends text frames; ignore binary.
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            webSocket.close(code, reason)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            Log.i(TAG, "ws closed: $code $reason")
            isConnected = false
            stopHeartbeat()
            listener.onDisconnected(code, reason)
            if (shouldReconnect) scheduleReconnect()
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: okhttp3.Response?) {
            Log.w(TAG, "ws failure: ${t.message}")
            isConnected = false
            stopHeartbeat()
            this@HomeCenterWebSocket.webSocket = null
            listener.onError(t, reconnectAttempt + 1)
            onErrorReport?.invoke(t)
            if (shouldReconnect) scheduleReconnect()
        }
    }

    companion object {
        private const val TYPE_HEARTBEAT = "heartbeat"
        private const val TYPE_SUBSCRIBE = "subscribe"
        private const val TYPE_UNSUBSCRIBE = "unsubscribe"
        private const val NORMAL_CLOSURE = 1000
    }
}

// =====================================================================================
// 5.5 Network monitor — reconnect on connectivity changes
// =====================================================================================

/**
 * Observes network availability and surfaces connectivity recovery to the
 * WebSocket so a WiFi↔cellular handoff or airplane-mode toggle reconnects
 * immediately instead of waiting out the backoff timer.
 * Requires android.permission.ACCESS_NETWORK_STATE.
 */
class NetworkMonitor(
    context: Context,
    private val onAvailable: () -> Unit
) : NetworkCallback() {

    private val connectivityManager =
        context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
    private var registered = false

    fun register() {
        if (registered) return
        registered = true
        connectivityManager.registerDefaultNetworkCallback(this)
    }

    fun unregister() {
        if (!registered) return
        registered = false
        connectivityManager.unregisterNetworkCallback(this)
    }

    override fun onAvailable(network: Network) {
        onAvailable()
    }
}