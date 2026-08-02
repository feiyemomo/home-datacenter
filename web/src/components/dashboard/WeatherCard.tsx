import React, { useMemo } from "react";
import {
    RefreshCw,
    Cloud,
    Sun,
    CloudSun,
    CloudRain,
    CloudSnow,
    CloudFog,
    CloudLightning,
    CloudDrizzle,
    Droplets,
    Wind,
    MapPin,
} from "lucide-react";
import { getWeather, wmoToIcon, type WeatherResponse } from "@/api/weather";
import { useCachedFetch } from "@/hooks/useCachedFetch";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";

/**
 * WeatherCard — top-of-dashboard weather summary, mirrors the
 * Android DashboardFragment's weather card. Calls GET /api/v1/weather
 * (proxied wttr.in j1) and renders current temp, "feels like",
 * WMO-code icon, location label, humidity + wind.
 *
 * The card degrades gracefully: if wttr.in is unreachable (the
 * backend's 5-min cache also helps), we show a compact "weather
 * unavailable" badge instead of a blank card.
 */
function WeatherCardInner() {
    // Cached fetch with silent background refresh every 10 min.
    // wttr.in updates ~every 10 min and the backend caches for 5,
    // so this rate is well-aligned with upstream freshness.
    // The cache makes re-mounts (e.g. switching tabs back to the
    // dashboard) instant — the previous weather payload is shown
    // immediately from sessionStorage while a refresh runs in
    // the background.
    const { data: weather, loading, error } = useCachedFetch<WeatherResponse>(
        "home.dashboard.weather",
        getWeather,
        { refetchMs: 10 * 60 * 1000 },
    );

    const cond = weather?.current_condition?.[0];
    const area = weather?.nearest_area?.[0];

    const code = cond?.weatherCode ? parseInt(cond.weatherCode, 10) : NaN;
    // wttr.in's WMO codes match the open-meteo table for 0..99, but
    // they also emit 113/116/119/122/143/176/200/227/230/248/260/263/266/281/284/293/296/299/302/305/308/311/314/317/320/323/326/329/332/335/338/350/353/356/359/362/365/368/371/374/377/386/389/392/395
    // (legacy Codes). We normalize the common ones to the WMO table.
    const wmo = useMemo(() => {
        if (Number.isNaN(code)) return { icon: "cloud", label: "—" };
        // Map wttr.in's 1xx codes down to WMO equivalents
        const m: Record<number, number> = {
            113: 0, 116: 2, 119: 3, 122: 3, 143: 45, 176: 51,
            200: 95, 227: 71, 230: 75, 248: 45, 260: 45,
            263: 51, 266: 53, 281: 53, 284: 55, 293: 61, 296: 61,
            299: 63, 302: 63, 305: 65, 308: 67, 311: 65, 314: 67,
            317: 67, 320: 71, 323: 71, 326: 73, 329: 75, 332: 75,
            335: 75, 338: 75, 350: 51, 353: 61, 356: 65, 359: 67,
            362: 71, 365: 73, 368: 71, 371: 73, 374: 75, 377: 75,
            386: 95, 389: 95, 392: 95, 395: 95,
        };
        const normalized = m[code] ?? code;
        return wmoToIcon(normalized);
    }, [code]);

    // Map icon name → lucide component
    const Icon = ({
        sun: Sun,
        cloud: Cloud,
        "cloud-sun": CloudSun,
        "cloud-rain": CloudRain,
        "cloud-snow": CloudSnow,
        "cloud-fog": CloudFog,
        "cloud-lightning": CloudLightning,
        "cloud-drizzle": CloudDrizzle,
    } as Record<string, typeof Sun>)[wmo.icon] ?? Cloud;

    const tempC = cond?.temp_C ? parseInt(cond.temp_C, 10) : null;
    const feelsC = cond?.FeelsLikeC ? parseInt(cond.FeelsLikeC, 10) : null;
    const humidity = cond?.humidity ? parseInt(cond.humidity, 10) : null;
    const windKmph = cond?.windspeedKmph ? parseInt(cond.windspeedKmph, 10) : null;
    const windDir = cond?.winddir16Point;
    const areaName = area?.areaName?.[0]?.value;
    const region = area?.region?.[0]?.value;

    return (
        <Card className="animate-fade-in relative overflow-hidden">
            <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-[rgb(var(--accent-warm)/0.15)] via-[rgb(var(--accent-primary)/0.05)] to-transparent" />
            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="flex items-center gap-2 text-xs tracking-wider text-fg-muted">
                    <Icon size={16} className="text-[rgb(var(--accent-warm))]" /> 天气
                </CardTitle>
                {areaName && (
                    <Badge variant="outline" className="text-[10px] gap-1">
                        <MapPin size={10} />
                        {areaName}{region ? ` · ${region}` : ""}
                    </Badge>
                )}
            </CardHeader>
            <CardContent className="relative">
                {loading ? (
                    <div className="flex items-center gap-2 text-fg-muted">
                        <RefreshCw size={14} className="animate-spin" />
                        <span className="text-xs">加载中…</span>
                    </div>
                ) : error || !cond ? (
                    <div className="flex items-center gap-2 text-fg-muted">
                        <Cloud size={20} className="opacity-50" />
                        <span className="text-xs">
                            {error ? error.message : "天气数据不可用"}
                        </span>
                    </div>
                ) : (
                    <div className="flex items-center gap-4">
                        {/* Big icon + temp */}
                        <div className="flex items-center gap-3">
                            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-[rgb(var(--accent-warm)/0.15)] ring-1 ring-inset ring-[rgb(var(--accent-warm)/0.3)]">
                                <Icon size={28} className="text-[rgb(var(--accent-warm))]" />
                            </div>
                            <div>
                                <div className="flex items-baseline gap-1">
                                    <span className="text-3xl font-semibold tracking-tight text-fg">
                                        {tempC ?? "—"}
                                    </span>
                                    <span className="text-sm text-fg-muted">°C</span>
                                </div>
                                <span className="text-xs text-fg-muted">{wmo.label}</span>
                            </div>
                        </div>
                        {/* Secondary stats */}
                        <div className="ml-auto grid grid-cols-3 gap-3 text-xs">
                            <div className="flex flex-col items-center">
                                <span className="text-fg-subtle">体感</span>
                                <span className="font-medium text-fg">
                                    {feelsC ?? "—"}°
                                </span>
                            </div>
                            <div className="flex flex-col items-center">
                                <Droplets size={12} className="text-fg-subtle" />
                                <span className="font-medium text-fg">
                                    {humidity ?? "—"}%
                                </span>
                            </div>
                            <div className="flex flex-col items-center">
                                <Wind size={12} className="text-fg-subtle" />
                                <span className="font-medium text-fg">
                                    {windKmph ?? "—"}
                                    <span className="text-fg-subtle"> km/h</span>
                                    {windDir ? ` ${windDir}` : ""}
                                </span>
                            </div>
                        </div>
                    </div>
                )}
            </CardContent>
        </Card>
    );
}

export const WeatherCard = React.memo(WeatherCardInner);