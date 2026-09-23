import os
import math
import logging
import numpy as np
import cv2

logger = logging.getLogger("pose_engine")

class PoseEngine:
    def __init__(self, model_dir: str):
        self.model_dir = model_dir
        self.model_path = os.path.join(self.model_dir, "yolov8n-pose.onnx")
        self.net = None
        self._load_model()

    def _load_model(self):
        if not os.path.exists(self.model_path):
            logger.warning("Pose model not found, pose detection will be disabled until downloaded.")
            return

        try:
            self.net = cv2.dnn.readNetFromONNX(self.model_path)
            self.net.setPreferableBackend(cv2.dnn.DNN_BACKEND_OPENCV)
            self.net.setPreferableTarget(cv2.dnn.DNN_TARGET_CPU)
            logger.info("Pose model (YOLOv8n-pose) loaded successfully.")
        except Exception as e:
            logger.error(f"Failed to load pose model: {e}")

    def _letterbox(self, img, new_shape=(640, 640), color=(114, 114, 114)):
        shape = img.shape[:2]
        r = min(new_shape[0] / shape[0], new_shape[1] / shape[1])
        new_unpad = (int(round(shape[1] * r)), int(round(shape[0] * r)))
        dw = new_shape[1] - new_unpad[0]
        dh = new_shape[0] - new_unpad[1]
        dw /= 2
        dh /= 2

        if shape[::-1] != new_unpad:
            img = cv2.resize(img, new_unpad, interpolation=cv2.INTER_LINEAR)
        top, bottom = int(round(dh - 0.1)), int(round(dh + 0.1))
        left, right = int(round(dw - 0.1)), int(round(dw + 0.1))
        img = cv2.copyMakeBorder(img, top, bottom, left, right, cv2.BORDER_CONSTANT, value=color)
        return img, r, (dw, dh)

    def detect(self, img_bgr: np.ndarray, conf_thresh=0.5):
        if self.net is None:
            return []

        h0, w0 = img_bgr.shape[:2]
        input_img, ratio, (dw, dh) = self._letterbox(img_bgr)
        blob = cv2.dnn.blobFromImage(input_img, 1.0 / 255.0, (640, 640), (0, 0, 0), swapRB=True, crop=False)
        self.net.setInput(blob)
        outputs = self.net.forward() # shape: (1, 56, 8400)

        # Transpose to (8400, 56)
        preds = np.transpose(outputs[0], (1, 0))

        boxes = []
        scores = []
        kpts_list = []

        for row in preds:
            box_score = row[4]
            if box_score < conf_thresh:
                continue

            cx, cy, w, h = row[0], row[1], row[2], row[3]
            # Map back to original image
            x1 = (cx - w / 2 - dw) / ratio
            y1 = (cy - h / 2 - dh) / ratio
            x2 = (cx + w / 2 - dw) / ratio
            y2 = (cy + h / 2 - dh) / ratio

            boxes.append([int(x1), int(y1), int(x2 - x1), int(y2 - y1)])
            scores.append(float(box_score))

            # 17 keypoints: [x, y, conf]
            raw_kpts = row[5:] # 51 elements
            kpts = []
            for i in range(17):
                kx = (raw_kpts[i * 3] - dw) / ratio
                ky = (raw_kpts[i * 3 + 1] - dh) / ratio
                kc = float(raw_kpts[i * 3 + 2])
                kpts.append([int(kx), int(ky), round(kc, 2)])
            kpts_list.append(kpts)

        if not boxes:
            return []

        indices = cv2.dnn.NMSBoxes(boxes, scores, conf_thresh, 0.45)
        results = []

        for idx in indices:
            i = idx if isinstance(idx, (int, np.integer)) else idx[0]
            box = boxes[i]
            score = scores[i]
            kpts = kpts_list[i]

            fall_analysis = self._analyze_fall(box, kpts)

            results.append({
                "box": box,
                "score": round(score, 3),
                "is_fall": fall_analysis["is_fall"],
                "fall_score": fall_analysis["fall_score"],
                "pose": fall_analysis["pose"],
                "aspect_ratio": fall_analysis["aspect_ratio"],
                "torso_angle": fall_analysis["torso_angle"],
                "keypoints": kpts
            })

        return results

    def _analyze_fall(self, box, kpts):
        _, _, w, h = box
        aspect_ratio = round(float(w) / max(1.0, float(h)), 2)

        # Keypoints:
        # 5: L-Shoulder, 6: R-Shoulder
        # 11: L-Hip, 12: R-Hip
        l_sh, r_sh = kpts[5], kpts[6]
        l_hip, r_hip = kpts[11], kpts[12]

        sh_valid = l_sh[2] > 0.3 and r_sh[2] > 0.3
        hip_valid = l_hip[2] > 0.3 and r_hip[2] > 0.3

        torso_angle = 90.0
        if sh_valid and hip_valid:
            sh_center = ((l_sh[0] + r_sh[0]) / 2.0, (l_sh[1] + r_sh[1]) / 2.0)
            hip_center = ((l_hip[0] + r_hip[0]) / 2.0, (l_hip[1] + r_hip[1]) / 2.0)

            dx = abs(sh_center[0] - hip_center[0])
            dy = abs(sh_center[1] - hip_center[1])
            # angle with horizontal axis
            torso_angle = round(math.degrees(math.atan2(dy, max(1e-3, dx))), 1)

        # Fall indicators:
        # 1. Aspect ratio: width > height (aspect_ratio >= 1.2)
        # 2. Torso angle: tilt close to horizontal (< 35 degrees)
        fall_score = 0.0

        if aspect_ratio >= 1.3:
            fall_score += 0.5
        elif aspect_ratio >= 1.0:
            fall_score += 0.25

        if torso_angle <= 30.0:
            fall_score += 0.5
        elif torso_angle <= 45.0:
            fall_score += 0.25

        fall_score = min(1.0, round(fall_score, 2))
        is_fall = fall_score >= 0.7

        if is_fall:
            pose = "fallen"
        elif aspect_ratio > 0.9 and torso_angle < 50:
            pose = "lying"
        elif aspect_ratio > 0.65:
            pose = "sitting"
        else:
            pose = "standing"

        return {
            "is_fall": is_fall,
            "fall_score": fall_score,
            "pose": pose,
            "aspect_ratio": aspect_ratio,
            "torso_angle": torso_angle
        }
