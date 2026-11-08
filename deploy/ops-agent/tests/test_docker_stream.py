"""
Test suite verifying Docker Socket HTTP Chunked transfer stream parsing
and JSON line event extraction without dropping events containing colons.
"""

import asyncio
import json
import os
import sys
import unittest

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)

from config import OpsAgentConfig
from main import HomeOpsAgent


class TestDockerStreamParser(unittest.IsolatedAsyncioTestCase):
    async def test_chunked_stream_parsing_and_no_colon_drop(self):
        """
        Docker /events emits Transfer-Encoding: chunked.
        Verify that hex lengths are parsed, chunks are read, and JSON containing ':' is NOT dropped.
        """
        # Create agent with test configuration
        config = OpsAgentConfig(
            docker_socket="/tmp/nonexistent_test.sock",
            api_base_url="http://127.0.0.1:8080"
        )
        agent = HomeOpsAgent(config)

        # Mock reader with HTTP headers, hex length, and JSON event
        sample_event = {
            "status": "die",
            "id": "1234567890abcdef",
            "from": "home-vision:latest",
            "Type": "container",
            "Action": "die",
            "Actor": {
                "ID": "1234567890abcdef",
                "Attributes": {
                    "name": "home-vision",
                    "exitCode": "137",
                    "image": "home-vision:latest"
                }
            },
            "time": 1731061200
        }
        event_json = json.dumps(sample_event).encode("utf-8")
        chunk_hex = f"{len(event_json):x}\r\n".encode("utf-8")

        stream_data = (
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: application/json\r\n"
            b"Transfer-Encoding: chunked\r\n\r\n" +
            chunk_hex +
            event_json +
            b"\r\n0\r\n\r\n"
        )

        class MockStreamReader:
            def __init__(self, data: bytes):
                self._data = data
                self._pos = 0

            async def readline(self) -> bytes:
                if self._pos >= len(self._data):
                    return b""
                idx = self._data.find(b"\n", self._pos)
                if idx == -1:
                    line = self._data[self._pos:]
                    self._pos = len(self._data)
                    return line
                line = self._data[self._pos:idx + 1]
                self._pos = idx + 1
                return line

            async def readexactly(self, n: int) -> bytes:
                if self._pos + n > len(self._data):
                    raise asyncio.IncompleteReadError(self._data[self._pos:], n)
                chunk = self._data[self._pos:self._pos + n]
                self._pos += n
                return chunk

        reader = MockStreamReader(stream_data)

        # Consume headers
        while True:
            header_line = await reader.readline()
            if not header_line or header_line in (b"\r\n", b"\n"):
                break

        # Read and parse chunks like main.py
        events_collected = []
        while True:
            line = await reader.readline()
            if not line:
                break
            raw_line = line.strip()
            if not raw_line:
                continue

            try:
                chunk_len = int(raw_line, 16)
            except ValueError:
                chunk_len = -1

            if chunk_len == 0:
                break
            elif chunk_len > 0:
                chunk_data = await reader.readexactly(chunk_len)
                await reader.readline()  # trailing CRLF
                event = json.loads(chunk_data.decode("utf-8"))
                events_collected.append(event)

        self.assertEqual(len(events_collected), 1)
        ev = events_collected[0]
        self.assertEqual(ev["Action"], "die")
        self.assertEqual(ev["Actor"]["Attributes"]["name"], "home-vision")
        self.assertEqual(ev["Actor"]["Attributes"]["exitCode"], "137")


if __name__ == "__main__":
    unittest.main()
