package com.example.homecenter

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import okhttp3.OkHttpClient

/**
 * Foreground service that hosts the WebSocket so the realtime channel
 * survives the app being backgrounded (Android can otherwise kill a
 * backgrounded process, dropping the WS and freezing the live feed).
 *
 * Lifecycle:
 *   START_STICKY            — if the system reclaims the service, restart it
 *                             with a null intent so the WS comes back up.
 *   NetworkMonitor          — reconnect immediately on WiFi/cellular recovery.
 *   ClientErrorReporter     — surface WS failures to the dashboard log pane.
 *   TokenStore              — the JWT / URL come from persistent storage, no
 *                             re-bind on every service start.
 */
class HomeCenterService : Service() {

    private val ioScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var okHttp: OkHttpClientHolder? = null
    private var webSocket: HomeCenterWebSocket? = null
    private var networkMonitor: NetworkMonitor? = null
    private var errorReporter: ClientErrorReporter? = null

    override fun onCreate() {
        super.onCreate()
        setupNotificationChannels()
        startAsForeground()
        startRealtime()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int = START_STICKY

    override fun onDestroy() {
        networkMonitor?.unregister()
        networkMonitor = null
        webSocket?.disconnect()
        webSocket = null
        ioScope.cancel()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun setupNotificationChannels() {
        val nm = getSystemService(NotificationManager::class.java)

        // Low-priority channel for persistent foreground service icon
        val serviceChannel = NotificationChannel(
            CHANNEL_ID,
            "Home Datacenter 运行服务",
            NotificationManager.IMPORTANCE_LOW
        ).apply { setShowBadge(false) }
        nm.createNotificationChannel(serviceChannel)

        // High-priority channel for critical safety & vision alerts
        val safetyChannel = NotificationChannel(
            CHANNEL_SAFETY_ID,
            "安全与跌倒告警",
            NotificationManager.IMPORTANCE_HIGH
        ).apply {
            description = "摄像头异常跌倒与安防紧急事件提醒"
            enableVibration(true)
            vibrationPattern = longArrayOf(0, 500, 200, 500)
            setShowBadge(true)
        }
        nm.createNotificationChannel(safetyChannel)
    }

    private fun startAsForeground() {
        val contentIntent = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        val notification: Notification = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("Home Datacenter")
            .setContentText("实时连接保持中")
            .setSmallIcon(android.R.drawable.ic_menu_compass)
            .setContentIntent(contentIntent)
            .setOngoing(true)
            .build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        } else {
            startForeground(NOTIFICATION_ID, notification, 0)
        }
    }

    private fun showSafetyNotification(title: String, content: String, notificationId: Int) {
        val intent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP
        }
        val pendingIntent = PendingIntent.getActivity(
            this,
            notificationId,
            intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )
        val notification = Notification.Builder(this, CHANNEL_SAFETY_ID)
            .setContentTitle(title)
            .setContentText(content)
            .setSmallIcon(android.R.drawable.ic_dialog_alert)
            .setContentIntent(pendingIntent)
            .setAutoCancel(true)
            .build()

        getSystemService(NotificationManager::class.java).notify(notificationId, notification)
    }

    private fun startRealtime() {
        val store = TokenStore(this)
        val token = store.token
        if (token.isEmpty()) {
            Log.w(TAG, "no token stored — service idle until the app binds a device")
            return
        }

        val baseUrl = store.baseUrl
        val wsUrl = store.wsUrl

        okHttp = OkHttpClientHolder(HomeCenterFactory.okHttpClient(enableVerboseLogging = false))
        val client = okHttp?.client ?: return
        val repo = HomeCenterRepository(HomeCenterFactory.createApi(baseUrl, client))

        errorReporter = ClientErrorReporter(token, repo, ioScope)

        val ws = HomeCenterWebSocket(
            client = client,
            wsUrl = wsUrl,
            token = token,
            scope = ioScope,
            listener = object : WsEventListener {
                override fun onConnected() {
                    Log.i(TAG, "background ws connected — subscribing to topics")
                    // Subscribe to camera alerts, fall detection and recognized persons
                    webSocket?.subscribe("camera.alert")
                    webSocket?.subscribe("camera.fall_detected")
                    webSocket?.subscribe("camera.person_recognized")
                }

                override fun onMessage(message: WsMessage) {
                    val topic = message.topic ?: ""
                    Log.d(TAG, "ws message received topic=$topic")
                    when (topic) {
                        "camera.fall_detected" -> {
                            val cam = message.payload?.get("camera_slug")?.toString()?.replace("\"", "") ?: "室内摄像头"
                            val isDelayed = message.payload?.get("delayed")?.asBoolean == true
                            val suffix = if (isDelayed) "（负载恢复后补测）" else ""
                            showSafetyNotification(
                                title = "🚨 紧急告警：检测到人员摔倒！$suffix",
                                content = "监控设备【$cam】检测到异常跌倒$suffix，请立即确认！",
                                notificationId = 2001
                            )
                        }
                        "camera.person_recognized" -> {
                            val name = message.payload?.get("name")?.toString()?.replace("\"", "") ?: "家庭成员"
                            showSafetyNotification(
                                title = "👤 视觉识别通知",
                                content = "摄像头识别到【$name】已归家",
                                notificationId = 2002
                            )
                        }
                    }
                }

                override fun onDisconnected(code: Int, reason: String?) {
                    Log.i(TAG, "background ws disconnected code=$code reason=$reason")
                }

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
        private const val TAG = "HomeCenter"
        private const val CHANNEL_ID = "home_datacenter"
        private const val CHANNEL_SAFETY_ID = "channel_safety_alerts"
        private const val NOTIFICATION_ID = 1001
    }
}

/** Small holder so the service can free the client on destroy. */
private class OkHttpClientHolder(val client: OkHttpClient)