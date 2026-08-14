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

    private fun startAsForeground() {
        val channel = NotificationChannel(
            CHANNEL_ID,
            "Home Datacenter",
            NotificationManager.IMPORTANCE_LOW
        ).apply { setShowBadge(false) }
        getSystemService(NotificationManager::class.java).createNotificationChannel(channel)

        // Tapping the notification returns the user to the main activity.
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
                    Log.i(TAG, "background ws connected")
                    // Re-subscribe to topics here (e.g. ws.subscribe("device.1")).
                }

                override fun onMessage(message: WsMessage) {
                    // Dispatch inbound realtime events.
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
        private const val NOTIFICATION_ID = 1001
    }
}

/** Small holder so the service can free the client on destroy. */
private class OkHttpClientHolder(val client: OkHttpClient)