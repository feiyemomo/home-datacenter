FROM python:3.11-slim

# Enforce resource-constrained lightweight execution
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

# Install lightweight dependencies (paho-mqtt for secure MQTT transport)
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY config.py proposal.py rule_engine.py main.py ./

CMD ["python", "main.py"]
