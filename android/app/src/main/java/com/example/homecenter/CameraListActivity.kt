package com.example.homecenter

import android.content.Intent
import android.graphics.Color
import android.os.Bundle
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.core.view.setPadding
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Camera list screen. Fetches GET /api/v1/cameras with the bound JWT and
 * exposes per-camera Live / Recordings entry points. All network work is tied
 * to the activity lifecycleScope so it is cancelled on destroy.
 */
class CameraListActivity : ComponentActivity() {

    private lateinit var store: TokenStore
    private lateinit var listContainer: LinearLayout
    private lateinit var statusView: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        store = TokenStore(this)
        setContentView(buildLayout())
        load()
    }

    private fun buildLayout(): android.view.View {
        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(20) }

        statusView = TextView(this).apply {
            text = "加载中…"
            textSize = 14f
        }

        Button(this).apply {
            text = "刷新"
            setOnClickListener { load() }
        }.let(root::addView)

        Button(this).apply {
            text = "⇐ 设置与绑定"
            setOnClickListener {
                startActivity(Intent(this@CameraListActivity, MainActivity::class.java))
            }
        }.let(root::addView)

        root.addView(statusView)

        listContainer = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
        }
        root.addView(ScrollView(this).apply { addView(listContainer) }, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f
        ))
        return root
    }

    private fun load() {
        val token = store.token
        if (token.isEmpty()) {
            statusView.text = "尚未绑定设备 —— 请先到「设置与绑定」填写 baseUrl / userId / AccessKey"
            return
        }
        statusView.text = "加载中…"
        listContainer.removeAllViews()
        val repo = HomeCenterRepository(
            HomeCenterFactory.createApi(store.baseUrl, HomeCenterFactory.okHttpClient())
        )
        lifecycleScope.launch {
            try {
                // Silently refresh token on app open so iat stays fresh and sliding expiration is active
                launch(Dispatchers.IO) {
                    try {
                        val freshToken = repo.refreshToken(token)
                        if (freshToken.isNotEmpty()) {
                            store.token = freshToken
                        }
                    } catch (_: Throwable) {
                        // Silent fallback — current token remains valid
                    }
                }
                val cameras = withContext(Dispatchers.IO) { repo.listCameras(token) }
                render(cameras)
            } catch (t: Throwable) {
                // Never swallow a cancellation: let the scope propagate it.
                if (t is kotlinx.coroutines.CancellationException) throw t
                statusView.text = "加载失败: ${t.javaClass.simpleName}: ${t.message}"
            }
        }
    }

    private fun render(cameras: List<Camera>) {
        if (cameras.isEmpty()) {
            statusView.text = "没有可用的摄像头"
            return
        }
        statusView.text = "共 ${cameras.size} 路摄像头（点「直播」看实时，点「录像」看回放）"
        cameras.forEach { cam ->
            listContainer.addView(cameraRow(cam))
        }
    }

    private fun cameraRow(cam: Camera): android.view.View {
        val row = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(8)
        }
        val online = cam.isOnline
        val info = TextView(this).apply {
            text = "${cam.name}   [${if (online) "在线" else "离线"}]  codec=${cam.codec}"
            textSize = 15f
            setTextColor(if (online) Color.rgb(0x2E, 0x7D, 0x32) else Color.GRAY)
        }
        row.addView(info)

        val btns = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
        Button(this).apply { text = "直播" }.apply {
            setOnClickListener {
                startActivity(Intent(this@CameraListActivity, LiveActivity::class.java).apply {
                    putExtra(LiveActivity.EXTRA_CAMERA_ID, cam.id)
                    putExtra(LiveActivity.EXTRA_CAMERA_NAME, cam.name)
                })
            }
        }.let(btns::addView)
        Button(this).apply { text = "录像" }.apply {
            setOnClickListener {
                startActivity(Intent(this@CameraListActivity, RecordingsActivity::class.java).apply {
                    putExtra(RecordingsActivity.EXTRA_CAMERA_ID, cam.id)
                    putExtra(RecordingsActivity.EXTRA_CAMERA_NAME, cam.name)
                })
            }
        }.let(btns::addView)
        row.addView(btns)
        return row
    }

    @Suppress("unused")
    private fun toast(msg: String) = Toast.makeText(this, msg, Toast.LENGTH_SHORT).show()
}