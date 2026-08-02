/** Format a detection label for display. */
export function formatLabel(label: string): string {
    const known: Record<string, string> = {
        person: "人员",
        car: "车辆",
        truck: "卡车",
        bus: "公交车",
        bicycle: "自行车",
        motorcycle: "摩托车",
        dog: "狗",
        cat: "猫",
        bird: "鸟",
    };
    return known[label] ?? label;
}

/** Format a confidence (0..1) as a percentage string. */
export function formatConfidence(c: number): string {
    if (!Number.isFinite(c) || c < 0) return "—";
    return `${Math.round(c * 100)}%`;
}

/** Format a unix timestamp as a locale time string. */
export function formatAlertTime(ts: number): string {
    if (!ts) return "—";
    return new Date(ts * 1000).toLocaleString();
}