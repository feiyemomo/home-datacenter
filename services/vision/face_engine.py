import os
import json
import logging
import numpy as np
import cv2

logger = logging.getLogger("face_engine")

class FaceEngine:
    def __init__(self, model_dir: str, data_dir: str):
        self.model_dir = model_dir
        self.data_dir = data_dir
        os.makedirs(self.data_dir, exist_ok=True)
        self.db_path = os.path.join(self.data_dir, "faces.json")

        self.det_model_path = os.path.join(self.model_dir, "face_detection_yunet_2023mar.onnx")
        self.rec_model_path = os.path.join(self.model_dir, "face_recognition_sface_2021dec.onnx")

        self.detector = None
        self.recognizer = None
        self.persons = [] # list of {"name": str, "feature": np.ndarray}

        self._load_models()
        self._load_db()

    def _load_models(self):
        if not os.path.exists(self.det_model_path) or not os.path.exists(self.rec_model_path):
            logger.warning("Face models not found, face recognition will be disabled until downloaded.")
            return

        try:
            # FaceDetectorYN: model, config, input_size, score_thresh, nms_thresh, top_k
            self.detector = cv2.FaceDetectorYN.create(
                self.det_model_path,
                "",
                (320, 320),
                score_threshold=0.7,
                nms_threshold=0.3,
                top_k=5000
            )
            # FaceRecognizerSF: model, config
            self.recognizer = cv2.FaceRecognizerSF.create(
                self.rec_model_path,
                ""
            )
            logger.info("Face models (YuNet + SFace) loaded successfully.")
        except Exception as e:
            logger.error(f"Failed to initialize face detector/recognizer: {e}")

    def _load_db(self):
        self.persons = []
        if not os.path.exists(self.db_path):
            return
        try:
            with open(self.db_path, "r", encoding="utf-8") as f:
                data = json.load(f)
                for item in data.get("persons", []):
                    self.persons.append({
                        "name": item["name"],
                        "feature": np.array(item["feature"], dtype=np.float32)
                    })
            logger.info(f"Loaded {len(self.persons)} registered persons.")
        except Exception as e:
            logger.error(f"Failed to load faces database: {e}")

    def _save_db(self):
        try:
            data = {
                "persons": [
                    {
                        "name": p["name"],
                        "feature": p["feature"].tolist()
                    }
                    for p in self.persons
                ]
            }
            with open(self.db_path, "w", encoding="utf-8") as f:
                json.dump(data, f, ensure_ascii=False, indent=2)
        except Exception as e:
            logger.error(f"Failed to save faces database: {e}")

    def list_persons(self):
        return [{"name": p["name"]} for p in self.persons]

    def delete_person(self, name: str) -> bool:
        initial_len = len(self.persons)
        self.persons = [p for p in self.persons if p["name"] != name]
        if len(self.persons) != initial_len:
            self._save_db()
            return True
        return False

    def register_person(self, name: str, img_bgr: np.ndarray) -> bool:
        if self.detector is None or self.recognizer is None:
            raise RuntimeError("Face recognition engine not initialized")

        h, w = img_bgr.shape[:2]
        self.detector.setInputSize((w, h))
        _, faces = self.detector.detect(img_bgr)

        if faces is None or len(faces) == 0:
            raise ValueError("No face detected in the image")

        # Pick the largest face in image
        largest_face = max(faces, key=lambda f: f[2] * f[3])
        aligned = self.recognizer.alignCrop(img_bgr, largest_face)
        feature = self.recognizer.feature(aligned)

        # Update or append
        self.delete_person(name)
        self.persons.append({
            "name": name,
            "feature": feature
        })
        self._save_db()
        return True

    def recognize(self, img_bgr: np.ndarray, cosine_thresh=0.363):
        if self.detector is None or self.recognizer is None:
            return []

        h, w = img_bgr.shape[:2]
        self.detector.setInputSize((w, h))
        _, faces = self.detector.detect(img_bgr)

        if faces is None or len(faces) == 0:
            return []

        results = []
        for face in faces:
            box = [int(face[0]), int(face[1]), int(face[2]), int(face[3])]
            det_score = float(face[14])
            if det_score < 0.6:
                continue

            aligned = self.recognizer.alignCrop(img_bgr, face)
            feat = self.recognizer.feature(aligned)

            best_match = None
            best_dist = 999.0

            for p in self.persons:
                dist = self.recognizer.match(feat, p["feature"], cv2.FaceRecognizerSF_FR_COSINE)
                if dist < best_dist:
                    best_dist = dist
                    best_match = p["name"]

            matched = best_dist <= cosine_thresh and best_match is not None
            # similarity = 1 - distance
            confidence = max(0.0, min(1.0, 1.0 - best_dist)) if best_match else 0.0

            results.append({
                "box": box,
                "det_score": round(det_score, 3),
                "matched": matched,
                "name": best_match if matched else "unknown",
                "similarity": round(confidence, 3),
                "distance": round(float(best_dist), 4)
            })

        return results
