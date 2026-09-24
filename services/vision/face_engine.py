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
                score_threshold=0.45,
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

        faces = None
        img_for_crop = img_bgr
        h, w = img_bgr.shape[:2]

        # Step 1: Detect on original image with threshold 0.45
        self.detector.setScoreThreshold(0.45)
        self.detector.setInputSize((w, h))
        _, faces = self.detector.detect(img_bgr)

        # Step 2: If no face found, retry with lower threshold 0.35
        if faces is None or len(faces) == 0:
            self.detector.setScoreThreshold(0.35)
            _, faces = self.detector.detect(img_bgr)

        # Step 3: If still no face, try scaled image (optimal receptive field for 1080p/2K/4K CCTV frames)
        if faces is None or len(faces) == 0:
            max_dim = max(h, w)
            if max_dim > 1024:
                scale = 1024.0 / max_dim
                nw, nh = int(w * scale), int(h * scale)
                resized = cv2.resize(img_bgr, (nw, nh), interpolation=cv2.INTER_AREA)
                self.detector.setInputSize((nw, nh))
                _, faces_resized = self.detector.detect(resized)
                if faces_resized is not None and len(faces_resized) > 0:
                    faces = faces_resized
                    img_for_crop = resized

        # Step 4: If still no face, try CLAHE contrast enhancement for backlit/dim surveillance scenes
        if faces is None or len(faces) == 0:
            lab = cv2.cvtColor(img_bgr, cv2.COLOR_BGR2LAB)
            l, a, b = cv2.split(lab)
            clahe = cv2.createCLAHE(clipLimit=2.0, tileGridSize=(8, 8))
            cl = clahe.apply(l)
            enhanced = cv2.cvtColor(cv2.merge((cl, a, b)), cv2.COLOR_LAB2BGR)
            self.detector.setInputSize((w, h))
            _, faces = self.detector.detect(enhanced)
            if faces is not None and len(faces) > 0:
                img_for_crop = enhanced

        # Reset detector score threshold and input size back to 0.45
        self.detector.setScoreThreshold(0.45)
        self.detector.setInputSize((w, h))

        if faces is None or len(faces) == 0:
            raise ValueError("No face detected in the image")

        # Pick the largest face in image
        largest_face = max(faces, key=lambda f: f[2] * f[3])
        aligned = self.recognizer.alignCrop(img_for_crop, largest_face)
        feature = self.recognizer.feature(aligned)

        # Update or append
        self.delete_person(name)
        self.persons.append({
            "name": name,
            "feature": feature
        })
        self._save_db()
        logger.info(f"Successfully registered person '{name}' with face box: {largest_face[:4].tolist()}")
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
            if det_score < 0.4:
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
