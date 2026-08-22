/*
 * PlayerKit.kt
 * ------------------------------------------------------------------
 * Media helpers for the Home Datacenter Android video client.
 *
 * - HevcSupport.hevcDecodable()  : device-side HEVC decoder probe (the
 *   Android analog of the web front-end's canDecodeHEVC()).
 * - PlayerKit.httpDataSource()   : a media3 data-source factory that stamps
 *   `Authorization: Bearer <jwt>` onto every HLS / progressive media request,
 *   so go2rtc (fronted by the nginx auth_request gate) and the /stream.of the
 *   DV /file handlers accept playback without any cookie juggling.
 */

package com.example.homecenter

import android.content.Context
import android.media.MediaCodecList
import android.media.MediaFormat
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.datasource.DefaultHttpDataSource
import androidx.media3.exoplayer.hls.HlsMediaSource
import androidx.media3.exoplayer.source.ProgressiveMediaSource
import androidx.media3.common.MediaItem

private const val TAG = "HomeCenter.PlayerKit"

/** Device-side HEVC hardware/software decoder capability probe. */
object HevcSupport {

    /** True when the device has a decoder for HEVC (hvc1/hev1) video. */
    fun hevcDecodable(): Boolean = try {
        val codecInfos = MediaCodecList(MediaCodecList.REGULAR_CODECS).codecInfos
        codecInfos.any { info ->
            !info.isEncoder && info.supportedTypes.contains(MediaFormat.MIMETYPE_VIDEO_HEVC)
        }
    } catch (t: Throwable) {
        // Very old devices / restricted profiles.
        false
    }
}

/** Builds ExoPlayer data sources / players authenticated as the bound JWT. */
object PlayerKit {

    /**
     * A [DefaultHttpDataSource.Factory] that adds the Authorization header to
     * every media request. Reused for both HLS (playlist + segments) and
     * progressive fMP4 (/stream, /file).
     */
    fun httpDataSource(auth: String): DefaultHttpDataSource.Factory =
        DefaultHttpDataSource.Factory()
            .setConnectTimeoutMs(15_000)
            .setReadTimeoutMs(30_000)
            .setDefaultRequestProperties(mapOf("Authorization" to "Bearer $auth"))

    fun buildPlayer(context: Context): ExoPlayer = ExoPlayer.Builder(context).build()

    /** HLS media source for live / HEVC-HLS streams (auth on every request). */
    fun hlsSource(auth: String, url: String): HlsMediaSource =
        HlsMediaSource.Factory(httpDataSource(auth))
            .setAllowChunklessPreparation(true)
            .createMediaSource(MediaItem.fromUri(url))

    /** Progressive media source (fragmented MP4) for recording playback. */
    fun progressiveSource(auth: String, url: String): ProgressiveMediaSource =
        ProgressiveMediaSource.Factory(httpDataSource(auth))
            .createMediaSource(MediaItem.fromUri(url))
}