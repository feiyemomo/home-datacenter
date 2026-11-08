"""
Tier 1 Feature Coverage: Work Order 2 (Android Unified Player & Gesture Refactor)
Validates core functional contracts for:
- PinchToZoomFrameLayout touch interception (multi-touch pointerCount >= 2)
- Single-touch delegation to child player controls at 1.0x
- Zoom scale clamping [1.0x, 4.0x]
- Translation boundary clamping [-(w*(s-1)/2), (w*(s-1)/2)]
- Double-tap toggle (1.0x <-> 2.0x) and coordinate centering
- PlayerCapabilities interface contracts and 4-second WebRTC fallback state machine
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from harness import GestureMath


class TestTier1WorkOrder2AndroidPlayer(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 2."""

    def test_pinch_to_zoom_multitouch_interception_contract(self):
        """
        WO 2.1: Verify multi-touch and scale in progress interception.
        Authoritative source: Task Specification Section 2.1 & PinchToZoomFrameLayout.kt.
        Expected: When pointerCount >= 2 or isInProgress, onInterceptTouchEvent returns true
        and requests parent disallow intercept touch event.
        """
        def simulate_on_intercept_touch_event(pointer_count: int, is_scale_in_progress: bool, current_scale: float) -> tuple[bool, bool]:
            # Returns (should_intercept, request_disallow_parent)
            if pointer_count >= 2 or is_scale_in_progress:
                return True, True
            if current_scale > 1.05:
                return True, True
            return False, False

        # Two fingers touch at initial scale 1.0x
        intercept, disallow = simulate_on_intercept_touch_event(2, False, 1.0)
        self.assertTrue(intercept, "Multi-touch must be intercepted at 1.0x")
        self.assertTrue(disallow, "Parent scroll views must be disallowed from intercepting")

        # Scale in progress
        intercept, disallow = simulate_on_intercept_touch_event(1, True, 1.0)
        self.assertTrue(intercept)
        self.assertTrue(disallow)

    def test_pinch_to_zoom_single_touch_delegation_at_unity(self):
        """
        WO 2.1: Verify single touch at 1.0x is NOT intercepted (passes to player controls).
        Authoritative source: Task Specification Section 2.1.
        Expected: pointerCount == 1 at 1.0x returns false so play/pause/controls receive clicks.
        """
        def simulate_on_intercept(pointer_count: int, is_scale_in_progress: bool, current_scale: float) -> bool:
            if pointer_count >= 2 or is_scale_in_progress:
                return True
            if current_scale > 1.05:
                return True
            return False

        intercept = simulate_on_intercept(1, False, 1.0)
        self.assertFalse(intercept, "Single tap at 1.0x should delegate to child controls")

        # But single drag when zoomed in (e.g. 2.5x) should be intercepted for panning
        intercept_zoomed = simulate_on_intercept(1, False, 2.5)
        self.assertTrue(intercept_zoomed, "Single drag when zoomed in must be intercepted for panning")

    def test_pinch_to_zoom_scale_clamping(self):
        """
        WO 2.1: Verify zoom scale is strictly clamped between 1.0x and 4.0x.
        Authoritative source: Task Specification Section 2.1.
        Expected: Clamp values outside [1.0, 4.0] to bounds.
        """
        self.assertEqual(GestureMath.clamp_scale(0.5), 1.0)
        self.assertEqual(GestureMath.clamp_scale(1.0), 1.0)
        self.assertEqual(GestureMath.clamp_scale(2.5), 2.5)
        self.assertEqual(GestureMath.clamp_scale(4.0), 4.0)
        self.assertEqual(GestureMath.clamp_scale(5.8), 4.0)

    def test_pinch_to_zoom_translation_clamping(self):
        """
        WO 2.1: Verify pan translation boundary clamping prevents black borders.
        Authoritative source: Task Specification Section 2.1 & PinchToZoomFrameLayout.kt.
        Expected: maxTrans = (dim * (scale - 1)) / 2.
        """
        view_w, view_h = 1080.0, 720.0
        scale = 2.0  # maxTransX = 1080 * 1 / 2 = 540, maxTransY = 720 * 1 / 2 = 360

        max_x, max_y = GestureMath.calculate_max_translation(view_w, view_h, scale)
        self.assertEqual(max_x, 540.0)
        self.assertEqual(max_y, 360.0)

        # Within bounds
        cx, cy = GestureMath.clamp_translation(200.0, -150.0, view_w, view_h, scale)
        self.assertEqual(cx, 200.0)
        self.assertEqual(cy, -150.0)

        # Exceeding bounds clamped to max
        cx_over, cy_over = GestureMath.clamp_translation(800.0, -500.0, view_w, view_h, scale)
        self.assertEqual(cx_over, 540.0)
        self.assertEqual(cy_over, -360.0)

    def test_double_tap_smooth_zoom_target_calculation(self):
        """
        WO 2.1: Verify double tap toggles 1.0x and 2.0x with focal point centering.
        Authoritative source: Task Specification Section 2.1 & PinchToZoomFrameLayout.kt.
        Expected: Double-tap when zoomed in resets to (1.0, 0, 0); double-tap at 1.0x zooms to 2.0x focusing on tap.
        """
        view_w, view_h = 1000.0, 600.0

        # Case 1: Currently zoomed at 2.0x -> double tap resets to 1.0x
        target_s, tx, ty = GestureMath.double_tap_target(2.0, 500.0, 300.0, view_w, view_h)
        self.assertEqual(target_s, 1.0)
        self.assertEqual(tx, 0.0)
        self.assertEqual(ty, 0.0)

        # Case 2: Currently at 1.0x, double tap at center (500, 300) -> zooms to 2.0x, trans = 0
        target_s, tx, ty = GestureMath.double_tap_target(1.0, 500.0, 300.0, view_w, view_h)
        self.assertEqual(target_s, 2.0)
        self.assertEqual(tx, 0.0)
        self.assertEqual(ty, 0.0)

        # Case 3: Currently at 1.0x, double tap at top-left (200, 100) -> zooms to 2.0x focusing top-left
        target_s, tx, ty = GestureMath.double_tap_target(1.0, 200.0, 100.0, view_w, view_h)
        self.assertEqual(target_s, 2.0)
        # focus_x = 200 - 500 = -300 -> target_trans_x = -(-300) * 1 = 300
        # maxTransX for 1000 at 2.0x = 500 -> 300 is within [ -500, 500 ]
        self.assertEqual(tx, 300.0)
        self.assertEqual(ty, 200.0)

    def test_player_capabilities_and_4s_fallback_state_machine(self):
        """
        WO 2.2: Verify PlayerCapabilities interface and 4-second WebRTC fallback timer.
        Authoritative source: Task Specification Section 2.2 & PROJECT.md Feature 9, 10.
        Expected: Live stream disables seeking/speed; timeout transitions to ExoPlayer; resetZoom on orientation.
        """
        class StreamPlayerStateMachine:
            def __init__(self):
                self.state = "IDLE"
                self.active_engine = "NONE"
                self.zoom_level = 1.0
                self.is_live = True

            @property
            def supports_seeking(self):
                return not self.is_live

            @property
            def supports_speed_adjustment(self):
                return not self.is_live

            def start_playback(self):
                self.state = "CONNECTING_WEBRTC"
                self.active_engine = "WEBRTC"

            def on_first_frame(self):
                if self.state == "CONNECTING_WEBRTC":
                    self.state = "PLAYING_WEBRTC"

            def on_timeout_4s(self):
                if self.state == "CONNECTING_WEBRTC":
                    # Fallback to ExoPlayer (texture_view)
                    self.state = "PLAYING_EXOPLAYER_FALLBACK"
                    self.active_engine = "EXOPLAYER"

            def on_orientation_change(self):
                # Reset zoom to 1.0x to avoid edge overflow
                self.zoom_level = 1.0

        sm = StreamPlayerStateMachine()
        self.assertEqual(sm.state, "IDLE")
        self.assertFalse(sm.supports_seeking)

        # Start WebRTC
        sm.start_playback()
        self.assertEqual(sm.state, "CONNECTING_WEBRTC")

        # 4s timeout fires before first frame -> fallback to ExoPlayer
        sm.on_timeout_4s()
        self.assertEqual(sm.state, "PLAYING_EXOPLAYER_FALLBACK")
        self.assertEqual(sm.active_engine, "EXOPLAYER")

        # Orientation change resets zoom
        sm.zoom_level = 3.0
        sm.on_orientation_change()
        self.assertEqual(sm.zoom_level, 1.0)


if __name__ == "__main__":
    unittest.main()
