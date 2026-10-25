import sys
import subprocess
import argparse
import json
import base64

def get_mqtt_password():
    # Read from NAS .env
    res = subprocess.run(["ssh", "nas", "grep '^MQTT_PASSWORD=' /vol1/docker/home-datacenter/.env | cut -d= -f2-"], capture_output=True, text=True)
    pwd = res.stdout.strip()
    return pwd if pwd else "homepass123"

def publish(topic, payload, qos=0, retain=False):
    pwd = get_mqtt_password()
    cmd = [
        "docker", "--context", "nas", "exec", "home-mosquitto",
        "mosquitto_pub",
        "-u", "home-datacenter",
        "-P", pwd,
        "-t", topic,
        "-m", payload,
        "-q", str(qos)
    ]
    if retain:
        cmd.append("-r")
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode == 0:
        print(f"[OK] Published to '{topic}': {payload}")
    else:
        print(f"[FAIL] Return code: {res.returncode}\n{res.stderr}", file=sys.stderr)

def subscribe(topic, count=5, timeout=10):
    pwd = get_mqtt_password()
    cmd = [
        "docker", "--context", "nas", "exec", "home-mosquitto",
        "mosquitto_sub",
        "-u", "home-datacenter",
        "-P", pwd,
        "-t", topic,
        "-C", str(count),
        "-W", str(timeout),
        "-v"
    ]
    print(f"[*] Subscribing to '{topic}' (waiting up to {timeout}s for {count} messages)...")
    try:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding="utf-8")
        stdout, stderr = proc.communicate(timeout=timeout + 5)
        if stdout:
            print(stdout.strip())
        if stderr and proc.returncode != 0:
            print(f"[STDERR] {stderr.strip()}", file=sys.stderr)
    except subprocess.TimeoutExpired:
        proc.kill()
        print("[-] Subscription timed out.")

def main():
    parser = argparse.ArgumentParser(description="MQTT probe tool for Home Datacenter (via NAS Mosquitto container)")
    subparsers = parser.add_subparsers(dest="action", required=True)

    pub_p = subparsers.add_parser("pub", help="Publish a message")
    pub_p.add_argument("-t", "--topic", required=True, help="Topic")
    pub_p.add_argument("-m", "--message", required=True, help="Message payload (JSON string or plain text)")
    pub_p.add_argument("-q", "--qos", type=int, default=0, help="QoS (0, 1, 2)")
    pub_p.add_argument("-r", "--retain", action="store_true", help="Retain message")

    sub_p = subparsers.add_parser("sub", help="Subscribe and capture messages")
    sub_p.add_argument("-t", "--topic", default="home-datacenter/#", help="Topic pattern")
    sub_p.add_argument("-c", "--count", type=int, default=5, help="Number of messages to read before exit")
    sub_p.add_argument("-w", "--timeout", type=int, default=10, help="Timeout in seconds")

    args = parser.parse_args()
    if args.action == "pub":
        publish(args.topic, args.message, qos=args.qos, retain=args.retain)
    elif args.action == "sub":
        subscribe(args.topic, count=args.count, timeout=args.timeout)

if __name__ == "__main__":
    main()
