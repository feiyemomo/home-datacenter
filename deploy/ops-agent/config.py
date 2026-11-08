"""
Configuration module for home-ops-agent.
Adheres strictly to the <= 50MB resident RAM constraint.
"""

import os
from dataclasses import dataclass


@dataclass
class OpsAgentConfig:
    name: str = "home-ops-agent"
    runtime: str = "python:3.11-slim"
    max_memory_mb: int = 50
    async_loop: str = "asyncio"

    # Infrastructure endpoints
    docker_socket: str = os.getenv("DOCKER_SOCKET", "/var/run/docker.sock")
    mqtt_broker: str = os.getenv("MQTT_BROKER", "home-mosquitto")
    mqtt_port: int = int(os.getenv("MQTT_PORT", "1883"))
    mqtt_topic: str = os.getenv("MQTT_TOPIC", "security/#")
    api_base_url: str = os.getenv("API_BASE_URL", "http://home-api:8080")
    new_api_url: str = os.getenv("NEW_API_URL", "http://home-new-api:3000/v1")
    new_api_token: str = os.getenv("NEW_API_TOKEN", "")
    new_api_model: str = os.getenv("NEW_API_MODEL", "agent-fast")
    db_path: str = os.getenv("DB_PATH", "/data/sqlite/app.db")

    # Tier 1 Thresholds
    auth_fail_threshold: int = 5
    auth_fail_window_sec: float = 300.0  # 5 minutes
    crash_escalation_threshold: int = 3
    crash_window_sec: float = 900.0      # 15 minutes
    disk_threshold_percent: float = 85.0

    # Low peak scheduling window (03:00 to 05:00 AM)
    low_peak_start_hour: int = 3
    low_peak_end_hour: int = 5

    @classmethod
    def load(cls) -> "OpsAgentConfig":
        return cls()
