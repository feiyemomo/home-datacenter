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
 * Multiple endpoints are tried in parallel because some are blocked by
 * the GFW in China or have AAAA records filtered by ISP DNS. We list
 * several to maximize the chance that at least one works:
 *   - ipv6.google.com: AAAA-only, but blocked by GFW
 *   - ipv6.test-ipv6.com: AAAA-only, China-accessible fallback
 *   - api6.ipify.org: AAAA-only, dedicated IPv6 endpoint
 *
 * Note: if all endpoints fail but the IPv6 Direct Probe to the server
 * succeeds, the caller should override the result to true — the probe
 * is the definitive test.
 *
 * Timeout is 3s per endpoint — on networks without IPv6, the failure is
 * usually immediate (DNS NXDOMAIN) rather than a hang.
 */
const IPV6_TEST_ENDPOINTS = [
    "https://ipv6.google.com/generate_204",
    "https://ipv6.test-ipv6.com/ip/?callback=test",
    "https://api6.ipify.org",
];

export async function checkClientIPv6(): Promise<boolean> {
    const results = await Promise.allSettled(
        IPV6_TEST_ENDPOINTS.map(async (url) => {
            const controller = new AbortController();
            const timeout = setTimeout(() => controller.abort(), 3000);
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

/**
 * Probe the server's IPv6 direct URL to test if IPv6 direct is reachable.
 * The server exposes GET /api/v1/network/probe (no auth) which returns
 * {"ok": true}. We fetch this via the IPv6 direct URL (different origin
 * from the dashboard) with mode:'no-cors' — we only care about TCP
 * connectivity, not the response body.
 *
 * Returns:
 *   - number (>=0): RTT in ms — reachable
 *   - -1: unreachable (connection failed or timed out)
 *   - -2: mixed content blocked (HTTPS dashboard → HTTP probe URL)
 *
 * The mixed content case happens when the dashboard is accessed via
 * Cloudflare Tunnel (HTTPS) but the probe target is plain HTTP.
 * Browsers block HTTP subresource requests from HTTPS pages. The
 * mobile app (native HTTP client) doesn't have this limitation.
 */
export async function probeIPv6Direct(directUrl: string): Promise<number> {
    // Detect mixed content: HTTPS page fetching HTTP resource.
    // Browsers block this silently — the fetch throws immediately.
    if (
        typeof window !== "undefined" &&
        window.location.protocol === "https:" &&
        directUrl.startsWith("http:")
    ) {
        return -2;
    }

    const probeUrl = `${directUrl}/api/v1/network/probe`;
    const start = performance.now();
    try {
        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), 5000);
        await fetch(probeUrl, {
            signal: controller.signal,
            mode: "no-cors",
        });
        clearTimeout(timeout);
        return Math.round(performance.now() - start);
    } catch {
        return -1;
    }
}
