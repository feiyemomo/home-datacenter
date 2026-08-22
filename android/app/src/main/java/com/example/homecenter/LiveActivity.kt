package com.example.homecenter

import android.content.Intent
import android.os.Bundle
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.annotation.OptIn
import androidx.core.view.setPadding
import androidx.lifecycle.lifecycleScope
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Live view. Prefers the camera's native-HEVC passthrough HLS when the device
 * can decode HEVC (probed via [HevcSupport.hevcDecodable]); otherwise — or on
 * playback failure — falls back to the transcoded H.264 HLS. A button lets the
 * operator switch the active source and see which link is being used.
 */
@OptIn(UnstableApi::class)
class LiveActivity : ComponentActivity() {

    private lateinit var store: TokenStore
    private lateinit var statusLabel: TextView
    private lateinit var switchButton: Button

    private var player: ExoPlayer? = null
    private var playerView: PlayerView? = null

    /** Ordered live links: label → absolute URL (resolved against baseUrl). */
    private val sources = ArrayList<Pair<String, String>>()
    private var currentIndex = 0
    private var fallbackAttempted = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        store = TokenStore(this)
        setContentView(buildLayout())
        window.addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        fetchAndPlay()
    }

    private fun buildLayout(): View {
        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(12) }

        val top = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
        Button(this).apply { text = "⇐ 返回" }.apply {
            setOnClickListener { finish() }
        }.let(top::addView)
        Button(this).apply { text = "看录像" }.apply {
            setOnClickListener {
                startActivity(Intent(this@LiveActivity, RecordingsActivity::class.java).apply {
                    putExtra(RecordingsActivity.EXTRA_CAMERA_ID, cameraId())
                    putExtra(RecordingsActivity.EXTRA_CAMERA_NAME,
                        intent.getStringExtra(EXTRA_CAMERA_NAME) ?: "摄像头")
                })
            }
        }.let(top::addView)
        root.addView(top)

        statusLabel = TextView(this).apply { text = "加载直播源…"; textSize = 13f }
        root.addView(statusLabel)

        switchButton = Button(this).apply { text = "切换链路" }
        root.addView(switchButton)

        playerView = PlayerView(this).apply {
            useController = true
            keepScreenOn = true
            resizeMode = AspectRatioFrameLayout.RESIZE_MODE_FIT
        }
        root.addView(playerView, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f
        ))
        return root
    }

    private fun cameraId(): Long = intent.getLongExtra(EXTRA_CAMERA_ID, 0L)

    private fun fetchAndPlay() {
        val token = store.token
        if (token.isEmpty()) {
            statusLabel.text = "尚未绑定 —— 请先回「设置与绑定」"
            return
        }
        val repo = HomeCenterRepository(
            HomeCenterFactory.createApi(store.baseUrl, HomeCenterFactory.okHttpClient())
        )
        lifecycleScope.launch {
            try {
                val cameras = withContext(Dispatchers.IO) { repo.listCameras(token) }
                val cam = cameras.firstOrNull { it.id == cameraId() }
                if (cam == null || cam.stream == null) {
                    statusLabel.text = "未找到摄像头或没有直播源"
                    return@launch
                }
                buildSources(cam)
                if (sources.isEmpty()) return@launch
                initializePlayer(token)
                applySource()
            } catch (t: Throwable) {
                if (t is kotlinx.coroutines.CancellationException) throw t
                statusLabel.text = "加载失败: ${t.javaClass.simpleName}: ${t.message}"
            }
        }
    }

    private fun buildSources(cam: Camera) {
        sources.clear()
        val stream = cam.stream
        if (stream == null) {
            statusLabel.text = "该摄像头没有可用的直播源"
            switchButton.isEnabled = false
            return
        }
        // Only offer HEVC when the device actually has a decoder for it.
        if (stream.hlsHevcUrl.isNotEmpty() && HevcSupport.hevcDecodable()) {
            sources.add("HEVC-HLS (原生透传)" to store.resolveAbsolute(stream.hlsHevcUrl))
        } else if (stream.hlsHevcUrl.isNotEmpty()) {
            statusLabel.text = "设备不支持硬解 HEVC，将使用 H.264 转码链路"
        }
        if (stream.hlsUrl.isNotEmpty()) {
            sources.add("H.264-HLS (转码)" to store.resolveAbsolute(stream.hlsUrl))
        }
        if (sources.isEmpty()) {
            statusLabel.text = "该摄像头没有可用的 HLS 直播源"
            switchButton.isEnabled = false
        }
    }

    private fun initializePlayer(token: String) {
        if (player != null) return
        val p = PlayerKit.buildPlayer(this)
        p.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                statusLabel.text = "播放出错: ${error.message}"
                // Auto fall back to the other live link exactly once.
                if (!fallbackAttempted && sources.size > 1) {
                    fallbackAttempted = true
                    currentIndex = (currentIndex + 1) % sources.size
                    applySource()
                }
            }

            override fun onIsPlayingChanged(isPlaying: Boolean) {
                if (isPlaying) {
                    statusLabel.text = "正在播放 · ${sources.getOrNull(currentIndex)?.first ?: ""}"
                }
            }
        })
        player = p
        playerView?.player = p
        switchButton.setOnClickListener {
            if (sources.size > 1) {
                currentIndex = (currentIndex + 1) % sources.size
                applySource()
            }
        }
    }

    private fun applySource() {
        val p = player ?: return
        val auth = store.token
        if (auth.isEmpty()) return
        val (label, url) = sources[currentIndex]
        statusLabel.text = "切换中 · $label"
        p.setMediaSource(PlayerKit.hlsSource(auth, url))
        p.prepare()
        p.playWhenReady = true
        switchButton.text = "当前:$label · 切换"
    }

    override fun onDestroy() {
        player?.release()
        player = null
        playerView?.player = null
        super.onDestroy()
    }

    companion object {
        const val EXTRA_CAMERA_ID = "camera_id"
        const val EXTRA_CAMERA_NAME = "camera_name"
    }
}