package com.example.homecenter

import android.os.Bundle
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.view.setPadding
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Minimal launcher UI: enter the NAS base URL + your user_id/AccessKey, bind a
 * device token, then start/stop the foreground service that keeps the realtime
 * WebSocket alive in the background.
 *
 * The token and URLs are persisted in [TokenStore] so the service can start
 * standalone on later launches without re-binding.
 */
class MainActivity : AppCompatActivity() {

    private lateinit var store: TokenStore

    private lateinit var baseUrlInput: EditText
    private lateinit var userIdInput: EditText
    private lateinit var accessKeyInput: EditText
    private lateinit var logView: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        store = TokenStore(this)
        setContentView(buildLayout())

        baseUrlInput.setText(store.baseUrl)
        userIdInput.setText(store.userId.takeIf { it > 0 }?.toString() ?: "")
    }

    // ---- UI ------------------------------------------------------------------------

    private fun buildLayout(): android.view.View {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(24)
        }

        baseUrlInput = EditText(this).apply {
            hint = "Base URL (e.g. http://192.168.31.235:8080/)"
        }
        userIdInput = EditText(this).apply {
            hint = "User ID"
            inputType = android.text.InputType.TYPE_CLASS_NUMBER
        }
        accessKeyInput = EditText(this).apply {
            hint = "Access Key (64-char hex)"
            inputType = android.text.InputType.TYPE_CLASS_TEXT or
                android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD
        }

        val bindButton = Button(this).apply { text = "绑定并连接" }
        val serviceButton = Button(this).apply { text = "启动后台服务" }
        val stopButton = Button(this).apply { text = "停止后台服务" }

        bindButton.setOnClickListener { bind() }
        serviceButton.setOnClickListener { startService() }
        stopButton.setOnClickListener { stopService() }

        logView = TextView(this).apply {
            textSize = 12f
            setTextColor(0xFF333333.toInt())
        }

        root.addView(baseUrlInput)
        root.addView(userIdInput)
        root.addView(accessKeyInput)
        root.addView(bindButton)
        root.addView(serviceButton)
        root.addView(stopButton)
        root.addView(ScrollView(this).apply {
            addView(logView)
        }, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT,
            0,
            1f
        ))
        return root
    }

    // ---- actions -------------------------------------------------------------------

    private fun bind() {
        val baseUrl = baseUrlInput.text.toString().trim()
        val userId = userIdInput.text.toString().trim().toLongOrNull()
        val accessKey = accessKeyInput.text.toString().trim()
        if (baseUrl.isEmpty() || userId == null || accessKey.isEmpty()) {
            appendLog("请填写 baseUrl / userId / accessKey")
            return
        }

        // Derive the WS URL from the base URL for the same host.
        val wsUrl = baseUrl.trimEnd('/')
            .replaceFirst("^https".toRegex(), "wss")
            .replaceFirst("^http".toRegex(), "ws") + "/api/v1/ws"

        lifecycleScope.launch {
            appendLog("正在绑定 user #$userId ...")
            try {
                val token = withContext(Dispatchers.IO) {
                    val repo = HomeCenterRepository(HomeCenterFactory.createApi(baseUrl, HomeCenterFactory.okHttpClient()))
                    repo.bind(userId, accessKey)
                }
                store.baseUrl = baseUrl
                store.wsUrl = wsUrl
                store.userId = userId
                store.token = token
                appendLog("绑定成功，token 长度=${token.length}")
            } catch (t: Throwable) {
                appendLog("绑定失败: ${t.javaClass.simpleName}: ${t.message}")
            }
        }
    }

    private fun startService() {
        if (store.token.isEmpty()) {
            appendLog("尚无 token —— 请先绑定")
            return
        }
        startForegroundService(android.content.Intent(this, HomeCenterService::class.java))
        appendLog("后台服务已启动（${
            store.wsUrl
        }）")
    }

    private fun stopService() {
        stopService(android.content.Intent(this, HomeCenterService::class.java))
        appendLog("后台服务已停止")
    }

    private fun appendLog(line: String) {
        logView.append(line + "\n")
    }
}