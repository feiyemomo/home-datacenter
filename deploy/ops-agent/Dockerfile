FROM python:3.11-slim

# Enforce resource-constrained lightweight execution
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

# The daemon is designed with zero external heavy dependencies for minimal memory footprint (RAM <= 50MB)
COPY config.py proposal.py rule_engine.py main.py ./

CMD ["python", "main.py"]
