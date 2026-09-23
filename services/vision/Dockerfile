FROM python:3.11-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    libglib2.0-0 \
    libgomp1 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .
RUN python download_models.py

EXPOSE 8090
ENV PORT=8090
ENV DATA_DIR=/data/vision
ENV MODEL_DIR=/app/models

CMD ["python", "main.py"]
