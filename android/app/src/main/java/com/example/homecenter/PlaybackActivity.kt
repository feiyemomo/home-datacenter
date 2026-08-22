package com.example.homecenter

import android.os.Bundle
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.annotation.OptIn
import androidx.core.view.setPadding
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView

/**
 * Single-recording playback. Streams the 60-second segment via the fast
 * fragmented-MP4 `/stream` endpoint (server-side H.264 720p, first frame in
 * ~1-2s) and, on failure, falls back to the whole-segment `/file` endpoint
 * (slower full transcode, but works anywhere).
 */
@OptIn(UnstableApi::class)
class PlaybackActivity : ComponentActivity() {

    private lateinit var store: TokenStore
    private lateinit var statusLabel: TextView

    private var player: ExoPlayer? = null
    private var playerView: PlayerView? = null
    private var usedFileFallback = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        store = TokenStore(this)
        setContentView(buildLayout())
        window.addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        playStream()
    }

    private fun cameraId(): Long = intent.getLongExtra(EXTRA_CAMERA_ID, 0L)
    private fun recId(): Long = intent.getLongExtra(EXTRA_REC_ID, 0L)

    private fun buildLayout(): View {
        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(12) }

        val top = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
        Button(this).apply { text = "⇐ 返回" }.apply {
            setOnClickListener { finish() }
        }.let(top::addView)
        TextView(this).apply {
            text = intent.getStringExtra(EXTRA_REC_LABEL) ?: "录像回放"
            textSize = 14f
        }.let(top::addView)
        root.addView(top)

        statusLabel = TextView(this).apply { text = "缓冲中…"; textSize = 13f }
        root.addView(statusLabel)

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

    private fun mediaUrl(stream: Boolean): String {
        val base = "/api/v1/cameras/${cameraId()}/recordings/${recId()}"
        return store.resolveAbsolute(if (stream) "$base/stream" else "$base/file")
    }

    private fun playStream() {
        val auth = store.token
        if (auth.isEmpty()) {
            statusLabel.text = "尚未绑定 —— 请先回「设置与绑定」"
            return
        }
        val p = PlayerKit.buildPlayer(this)
        p.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                if (!usedFileFallback) {
                    usedFileFallback = true
                    statusLabel.text = "流式播放失败，改用整段文件：${error.message}"
                    p.setMediaSource(PlayerKit.progressiveSource(auth, mediaUrl(stream = false)))
                    p.prepare()
                    p.playWhenReady = true
                } else {
                    statusLabel.text = "播放失败: ${error.message}"
                }
            }

            override fun onIsPlayingChanged(isPlaying: Boolean) {
                if (isPlaying) {
                    statusLabel.text = if (usedFileFallback) "正在播放 · 整段文件" else "正在播放 · 流式"
                }
            }
        })
        player = p
        playerView?.player = p
        p.setMediaSource(PlayerKit.progressiveSource(auth, mediaUrl(stream = true)))
        p.prepare()
        p.playWhenReady = true
    }

    override fun onDestroy() {
        player?.release()
        player = null
        playerView?.player = null
        super.onDestroy()
    }

    companion object {
        const val EXTRA_CAMERA_ID = "camera_id"
        const val EXTRA_REC_ID = "rec_id"
        const val EXTRA_REC_LABEL = "rec_label"
    }
}