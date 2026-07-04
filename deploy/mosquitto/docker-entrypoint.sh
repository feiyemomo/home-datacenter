#!/bin/ash
# Mosquitto entrypoint for Docker Desktop / Windows bind mounts.
#
# The password and ACL files are bind-mounted from the host into
# /mosquitto/config. On Windows those files often end up with 777
# permissions that Mosquitto 2.x refuses to load. We copy them into
# the container's /mosquitto/data volume (a real Linux filesystem)
# and lock the permissions down before starting the broker.
set -e

CONFIG_DIR=/mosquitto/config
DATA_DIR=/mosquitto/data

if [ -f "$CONFIG_DIR/passwd" ]; then
    cp "$CONFIG_DIR/passwd" "$DATA_DIR/passwd"
    chown mosquitto:mosquitto "$DATA_DIR/passwd"
    chmod 600 "$DATA_DIR/passwd"
fi

if [ -f "$CONFIG_DIR/aclfile" ]; then
    cp "$CONFIG_DIR/aclfile" "$DATA_DIR/aclfile"
    chown mosquitto:mosquitto "$DATA_DIR/aclfile"
    chmod 600 "$DATA_DIR/aclfile"
fi

exec mosquitto -c "$CONFIG_DIR/mosquitto.conf"
