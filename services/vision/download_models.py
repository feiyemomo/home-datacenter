#!/usr/bin/env python3
import os
import sys
import time
import urllib.request

MODELS = [
    {
        "name": "face_detection_yunet_2023mar.onnx",
        "urls": [
            "https://media.githubusercontent.com/media/opencv/opencv_zoo/main/models/face_detection_yunet/face_detection_yunet_2023mar.onnx",
            "https://github.com/opencv/opencv_zoo/raw/main/models/face_detection_yunet/face_detection_yunet_2023mar.onnx"
        ],
        "min_size": 200000
    },
    {
        "name": "face_recognition_sface_2021dec.onnx",
        "urls": [
            "https://media.githubusercontent.com/media/opencv/opencv_zoo/main/models/face_recognition_sface/face_recognition_sface_2021dec.onnx",
            "https://github.com/opencv/opencv_zoo/raw/main/models/face_recognition_sface/face_recognition_sface_2021dec.onnx"
        ],
        "min_size": 10000000
    },
    {
        "name": "yolov8n-pose.onnx",
        "urls": [
            "https://huggingface.co/Xenova/yolov8n-pose/resolve/main/onnx/model.onnx",
            "https://hf-mirror.com/Xenova/yolov8n-pose/resolve/main/onnx/model.onnx"
        ],
        "min_size": 10000000
    }
]

def download_file(target_path, urls, min_size):
    if os.path.exists(target_path) and os.path.getsize(target_path) >= min_size:
        print(f"[OK] {os.path.basename(target_path)} already exists ({os.path.getsize(target_path)} bytes).", flush=True)
        return True

    print(f"Downloading {os.path.basename(target_path)}...", flush=True)
    tmp_path = target_path + ".tmp"

    for url in urls:
        print(f" Trying {url} ...", flush=True)
        for attempt in range(1, 4):
            try:
                req = urllib.request.Request(
                    url,
                    headers={"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"}
                )
                with urllib.request.urlopen(req, timeout=30) as resp, open(tmp_path, "wb") as f:
                    chunk_size = 1024 * 64
                    downloaded = 0
                    while True:
                        chunk = resp.read(chunk_size)
                        if not chunk:
                            break
                        f.write(chunk)
                        downloaded += len(chunk)
                if os.path.exists(tmp_path) and os.path.getsize(tmp_path) >= min_size:
                    os.replace(tmp_path, target_path)
                    print(f"[SUCCESS] Downloaded {os.path.basename(target_path)} ({os.path.getsize(target_path)} bytes)", flush=True)
                    return True
                else:
                    print(f" Attempt {attempt} failed: file size {os.path.getsize(tmp_path) if os.path.exists(tmp_path) else 0} < {min_size}", flush=True)
            except Exception as e:
                print(f" Attempt {attempt} error: {e}", flush=True)
                time.sleep(1)
            finally:
                if os.path.exists(tmp_path):
                    try:
                        os.remove(tmp_path)
                    except Exception:
                        pass
    return False

def main():
    model_dir = os.environ.get("MODEL_DIR", os.path.join(os.path.dirname(__file__), "models"))
    os.makedirs(model_dir, exist_ok=True)
    all_ok = True
    for item in MODELS:
        dest = os.path.join(model_dir, item["name"])
        if not download_file(dest, item["urls"], item["min_size"]):
            print(f"[ERROR] Failed to download {item['name']}", flush=True)
            all_ok = False

    if not all_ok:
        sys.exit(1)
    print("All models ready.", flush=True)

if __name__ == "__main__":
    main()
