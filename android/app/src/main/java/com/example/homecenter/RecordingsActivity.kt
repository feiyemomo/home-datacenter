package com.example.homecenter

import android.content.Intent
import android.os.Bundle
import android.view.View
import android.widget.Button
import android.widget.HorizontalScrollView
import android.widget.LinearLayout
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.core.view.setPadding
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.text.SimpleDateFormat
import java.util.Calendar
import java.util.Date
import java.util.Locale

/**
 * Recording timeline. Loads the last 7 days by default (matches the server's
 * `after/before` defaults) and pages further back through `after/before`.
 * Each 60-second segment opens [PlaybackActivity] which streams it via the
 * fast `/stream` endpoint (fMP4, ~1-2s first frame) and falls back to `/file`.
 */
class RecordingsActivity : ComponentActivity() {

    private lateinit var store: TokenStore
    private lateinit var container: LinearLayout
    private lateinit var statusView: TextView
    private lateinit var loadEarlierBtn: Button

    private val all = LinkedHashMap<Long, CameraRecording>() // rec.id → rec
    private var loaded = false
    private var loading = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        store = TokenStore(this)
        setContentView(buildLayout())
        load(true)
    }

    private fun cameraId(): Long = intent.getLongExtra(EXTRA_CAMERA_ID, 0L)
    private fun cameraName(): String = intent.getStringExtra(EXTRA_CAMERA_NAME) ?: "摄像头"

    private fun buildLayout(): View {
        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(12) }

        val top = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
        Button(this).apply { text = "⇐ 返回" }.apply {
            setOnClickListener { finish() }
        }.let(top::addView)
        Button(this).apply { text = "看直播" }.apply {
            setOnClickListener {
                startActivity(Intent(this@RecordingsActivity, LiveActivity::class.java).apply {
                    putExtra(LiveActivity.EXTRA_CAMERA_ID, cameraId())
                    putExtra(LiveActivity.EXTRA_CAMERA_NAME, cameraName())
                })
            }
        }.let(top::addView)
        root.addView(top)

        statusView = TextView(this).apply { text = "加载录像中…"; textSize = 13f }
        root.addView(statusView)

        loadEarlierBtn = Button(this).apply { text = "加载更早（7天）" }
        loadEarlierBtn.setOnClickListener { load(false) }
        root.addView(loadEarlierBtn)

        container = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        root.addView(container, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f
        ))
        return root
    }

    private fun load(initial: Boolean) {
        val token = store.token
        if (token.isEmpty()) {
            statusView.text = "尚未绑定 —— 请先回「设置与绑定」"
            return
        }
        if (loading) return
        loading = true
        statusView.text = if (initial) "加载录像中…" else "加载更早录像…"
        val repo = HomeCenterRepository(
            HomeCenterFactory.createApi(store.baseUrl, HomeCenterFactory.okHttpClient())
        )
        lifecycleScope.launch {
            try {
                val added = withContext(Dispatchers.IO) {
                    repo.listRecordings(
                        token,
                        cameraId(),
                        after = if (initial) null else oldestDayStart() - PAGE_SECONDS,
                        before = if (initial) null else oldestDayStart()
                    )
                }
                var fresh = 0
                for (r in added) if (!all.containsKey(r.id)) { all[r.id] = r; fresh++ }
                loaded = true
                statusView.text = "共 ${all.size} 个录像片段（本次新增 $fresh）· 点击片断播放"
                render()
            } catch (t: Throwable) {
                if (t is kotlinx.coroutines.CancellationException) throw t
                statusView.text = "加载失败: ${t.javaClass.simpleName}: ${t.message}"
            } finally {
                loading = false
            }
        }
    }

    /** Unix seconds at the start of the oldest loaded local day. */
    private fun oldestDayStart(): Long {
        val ids = all.keys
        if (ids.isEmpty()) return nowSec()
        val cal = Calendar.getInstance()
        cal.timeInMillis = ids.min()!! * 1000L
        cal.set(Calendar.HOUR_OF_DAY, 0); cal.set(Calendar.MINUTE, 0)
        cal.set(Calendar.SECOND, 0); cal.set(Calendar.MILLISECOND, 0)
        return cal.timeInMillis / 1000L
    }

    private fun nowSec(): Long = System.currentTimeMillis() / 1000L

    private fun render() {
        container.removeAllViews()
        val sorted = all.values.sortedBy { it.id }
        if (sorted.isEmpty()) {
            container.addView(TextView(this).apply {
                text = "该时段没有录像"
            })
            return
        }
        // Group by local date (yyyy-MM-dd).
        val dayFormat = SimpleDateFormat("yyyy-MM-dd", Locale.getDefault())
        val group = LinkedHashMap<String, MutableList<CameraRecording>>()
        for (r in sorted) {
            val key = dayFormat.format(Date(r.id * 1000L))
            group.getOrPut(key) { ArrayList() }.add(r)
        }
        for ((day, recs) in group) {
            container.addView(TextView(this).apply {
                text = "—— $day ——"
                textSize = 14f
            })
            val chips = LinearLayout(this).apply {
                orientation = LinearLayout.HORIZONTAL
                setPadding(4, 0, 4, 0)
            }
            val hm = SimpleDateFormat("HH:mm", Locale.getDefault())
            for (r in recs) {
                Button(this).apply {
                    text = hm.format(Date(r.id * 1000L))
                    setOnClickListener {
                        startActivity(Intent(this@RecordingsActivity, PlaybackActivity::class.java).apply {
                            putExtra(PlaybackActivity.EXTRA_CAMERA_ID, cameraId())
                            putExtra(PlaybackActivity.EXTRA_REC_ID, r.id)
                            putExtra(PlaybackActivity.EXTRA_REC_LABEL, "$day ${hm.format(Date(r.id * 1000L))}")
                        })
                    }
                }.let(chips::addView)
            }
            container.addView(HorizontalScrollView(this).apply { addView(chips) })
        }
    }

    companion object {
        const val EXTRA_CAMERA_ID = "camera_id"
        const val EXTRA_CAMERA_NAME = "camera_name"
        private const val PAGE_SECONDS = 7L * 86_400L
    }
}