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
    Thermometer,
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

function WeatherCardInner() {
    const { data: weather, loading, error } = useCachedFetch<WeatherResponse>(
        "home.dashboard.weather",
        getWeather,
        { refetchMs: 10 * 60 * 1000 },
    );

    const cond = weather?.current_condition?.[0];
    const area = weather?.nearest_area?.[0];

    const code = cond?.weatherCode ? parseInt(cond.weatherCode, 10) : NaN;
    const wmo = useMemo(() => {
        if (Number.isNaN(code)) return { icon: "cloud", label: "—" };
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
        <Card className="animate-fade-in relative overflow-hidden glass glass-glow card-lift">
            <CardHeader className="relative flex-row items-center justify-between pb-3">
                <CardTitle className="flex items-center gap-2 text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                    <Icon size={15} className="text-[rgb(var(--accent-warm))]" /> 天气
                </CardTitle>
                {areaName && (
                    <Badge variant="outline" className="gap-1 text-[10px] glass-subtle text-fg-muted/80">
                        <MapPin size={9} />
                        {areaName}{region ? ` · ${region}` : ""}
                    </Badge>
                )}
            </CardHeader>
            <CardContent className="relative">
                {loading ? (
                    <div className="flex items-center gap-3 text-fg-muted">
                        <RefreshCw size={16} className="animate-spin" />
                        <span className="text-xs">加载天气中…</span>
                    </div>
                ) : error || !cond ? (
                    <div className="flex items-center gap-3 text-fg-muted">
                        <Cloud size={22} className="opacity-40" />
                        <span className="text-xs">
                            {error ? error.message : "天气数据暂时不可用"}
                        </span>
                    </div>
                ) : (
                    <div className="flex items-center gap-5">
                        <div className="flex items-center gap-4">
                            <div className="relative flex h-14 w-14 items-center justify-center rounded-2xl bg-gradient-to-br from-[rgb(var(--accent-warm)/0.15)] to-[rgb(var(--accent-warm)/0.05)] ring-1 ring-inset ring-[rgb(var(--accent-warm)/0.15)] shadow-[0_4px_12px_rgb(var(--accent-warm)/0.08)] transition-transform duration-300 hover:scale-105">
                                <Icon size={28} className="text-[rgb(var(--accent-warm))]" />
                                <div className="absolute inset-0 rounded-2xl bg-gradient-to-t from-transparent to-white/10" />
                            </div>
                            <div>
                                <div className="flex items-baseline gap-1">
                                    <span className="text-5xl font-semibold tracking-tight text-fg">
                                        {tempC ?? "—"}
                                    </span>
                                    <span className="text-lg text-fg-muted">°C</span>
                                </div>
                                <span className="text-sm text-fg-muted">{wmo.label}</span>
                            </div>
                        </div>

                        <div className="h-10 w-px bg-[rgb(var(--border)/0.3)]" />

                        <div className="ml-auto grid grid-cols-3 gap-3 text-xs">
                            <div className="flex flex-col items-center gap-1">
                                <div className="flex items-center gap-1 text-fg-muted/80">
                                    <Thermometer size={11} />
                                    <span>体感</span>
                                </div>
                                <span className="text-base font-semibold text-fg">
                                    {feelsC ?? "—"}°
                                </span>
                            </div>
                            <div className="flex flex-col items-center gap-1">
                                <div className="flex items-center gap-1 text-fg-muted/80">
                                    <Droplets size={11} />
                                    <span>湿度</span>
                                </div>
                                <span className="text-base font-semibold text-fg">
                                    {humidity ?? "—"}<span className="text-xs text-fg-muted">%</span>
                                </span>
                            </div>
                            <div className="flex flex-col items-center gap-1">
                                <div className="flex items-center gap-1 text-fg-muted/80">
                                    <Wind size={11} />
                                    <span>风速</span>
                                </div>
                                <span className="text-base font-semibold text-fg">
                                    {windKmph ?? "—"}
                                    <span className="text-xs text-fg-muted"> km/h</span>
                                    {windDir ? <span className="ml-1 text-[10px] text-fg-muted">{windDir}</span> : ""}
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
