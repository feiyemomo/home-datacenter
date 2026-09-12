/**
 * Network topology detection utility.
 *
 * Unifies network topology checks across Dashboard, Network, and LiveVideo.
 *
 * Supported paths:
 *   - "lan":    localhost, 127.0.0.1, 192.168.x.x, 10.x.x.x, 172.16-31.x.x
 *   - "ipv6":   nas.feiyemomo.top (pure-AAAA DDNS) or IPv6 literals ([...])
 *   - "remote": everything else (e.g. Cloudflare Tunnel relay)
 */

export const NAS_DDNS_DOMAIN = "nas.feiyemomo.top";

export type ApiPathType = "lan" | "ipv6" | "remote";

export function detectApiPath(): ApiPathType {
    if (typeof window === "undefined") return "remote";
    const h = window.location.hostname;
    if (h === "localhost" || h === "127.0.0.1") return "lan";
    if (h.startsWith("192.168.") || h.startsWith("10.")) return "lan";
    if (/^172\.(1[6-9]|2[0-9]|3[01])\./.test(h)) return "lan";
    if (h === NAS_DDNS_DOMAIN) return "ipv6";
    if (/^\[[0-9a-f:]+\]$/i.test(h)) return "ipv6";
    return "remote";
}

/**
 * Reports whether the client is connecting remotely via a relay/tunnel
 * rather than direct LAN or IPv6.
 */
export function isRemoteAccess(): boolean {
    return detectApiPath() === "remote";
}

/**
 * Alias for isRemoteAccess for connection model views.
 */
export function isOnRelay(): boolean {
    return detectApiPath() === "remote";
}
