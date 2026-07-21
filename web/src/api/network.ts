import client from "./client";
import type { NetworkStatus, ServerEndpoint } from "@/types";

/**
 * Network capability report — IPv6, NAT, P2P, relay, and recommended
 * connection strategy.
 *
 * GET /api/v1/network/status
 *
 * Pass refresh=true to force a fresh detection (bypasses the cache).
 */
export async function getNetworkStatus(refresh = false): Promise<NetworkStatus> {
    const url = refresh ? "/network/status?refresh=true" : "/network/status";
    const { data } = await client.get<NetworkStatus>(url);
    return data as NetworkStatus;
}

/**
 * The server's own P2P endpoint (STUN-discovered public address).
 *
 * GET /api/v1/network/p2p/server-endpoint
 */
export async function getServerEndpoint(): Promise<ServerEndpoint> {
    const { data } = await client.get<ServerEndpoint>("/network/p2p/server-endpoint");
    return data as ServerEndpoint;
}

/**
 * Detect whether THIS CLIENT (the browser/device running the dashboard)
 * has IPv6 connectivity. This is independent of the server's IPv6 status.
 *
 * Tries multiple IPv6-only endpoints (domains with ONLY AAAA records, no
 * A record). If the client has no IPv6, DNS resolution fails or the
 * connection times out, and the fetch throws. With mode:'no-cors' the
 * response is opaque but the promise resolves on a successful TCP+TLS
 * handshake.
 *
 * Multiple endpoints are tried in parallel because ipv6.google.com is
 * blocked by the GFW in China — we need China-accessible fallbacks
 * (ipv6.test-ipv6.com, mirrors6.tuna.tsinghua.edu.cn).
 *
 * Timeout is 3s per endpoint — on networks without IPv6, the failure is
 * usually immediate (DNS NXDOMAIN) rather than a hang.
 *
 * v1.8.3: added mirrors6.tuna.tsinghua.edu.cn (Tsinghua TUNA, China),
 * and ipv6.baidu.com as additional fallbacks. The previous only-two-endpoint
 * design meant users on Chinese networks behind GFW got false "no IPv6"
 * results because both endpoints were blocked or slow.
 */
const IPV6_TEST_ENDPOINTS = [
    "https://ipv6.test-ipv6.com/ip/?callback=test",
    "https://mirrors6.tuna.tsinghua.edu.cn/",
    "https://ipv6.baidu.com/",
    "https://ipv6.google.com/generate_204",
];

export async function checkClientIPv6(): Promise<boolean> {
    const results = await Promise.allSettled(
        IPV6_TEST_ENDPOINTS.map(async (url) => {
            const controller = new AbortController();
            const timeout = setTimeout(() => controller.abort(), 4000);
            try {
                await fetch(url, {
                    signal: controller.signal,
                    mode: "no-cors",
                });
            } finally {
                clearTimeout(timeout);
            }
        }),
    );
    // If ANY endpoint connected, the client has IPv6.
    return results.some((r) => r.status === "fulfilled");
}
