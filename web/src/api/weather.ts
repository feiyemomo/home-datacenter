import client from "./client";

/**
 * Weather proxy — wraps GET /api/v1/weather, which the backend
 * proxies from wttr.in (j1 format) with a 5-min server-side cache.
 *
 * The dashboard uses this to render a WeatherCard at the top of
 * /dashboard, mirroring the Android app's DashboardFragment.
 *
 * wttr.in's j1 schema (subset we care about):
 *
 *   {
 *     "current_condition": [{
 *       "temp_C": "20",
 *       "FeelsLikeC": "19",
 *       "weatherCode": "113",        // WMO code
 *       "weatherDesc": [{"value": "Sunny"}],
 *       "humidity": "45",
 *       "windspeedKmph": "5",
 *       "winddir16Point": "NW"
 *     }],
 *     "nearest_area": [{
 *       "areaName": [{"value": "Baoji"}],
 *       "region":   [{"value": "Shaanxi"}],
 *       "country":  [{"value": "China"}]
 *     }]
 *   }
 *
 * All numeric fields arrive as strings — wttr.in's serializer emits
 * everything as strings. We coerce at the component layer.
 */
export interface WeatherResponse {
    current_condition: Array<{
        temp_C: string;
        temp_F?: string;
        FeelsLikeC?: string;
        FeelsLikeF?: string;
        humidity?: string;
        weatherCode?: string;
        weatherDesc?: Array<{ value: string }>;
        weatherIconUrl?: Array<{ value: string }>;
        windspeedKmph?: string;
        windspeedMiles?: string;
        winddir16Point?: string;
        winddirDegree?: string;
        pressure?: string;
        visibility?: string;
        cloudcover?: string;
        uvIndex?: string;
    }>;
    nearest_area?: Array<{
        areaName: Array<{ value: string }>;
        region?: Array<{ value: string }>;
        country?: Array<{ value: string }>;
        latitude?: string;
        longitude?: string;
    }>;
}

export async function getWeather(): Promise<WeatherResponse> {
    const { data } = await client.get<WeatherResponse>("/weather");
    return data as WeatherResponse;
}

/**
 * WMO weather code → lucide icon name + short label (Chinese).
 *
 * Reference: https://open-meteo.com/en/docs (WMO code table).
 * wttr.in uses the same numeric codes.
 *
 * Returns a tuple of [iconName, label]. The component maps the
 * iconName to a lucide-react component.
 */
export function wmoToIcon(code: number): { icon: string; label: string } {
    // Group by visual similarity; Android's weatherIconFor uses
    // the same buckets (see DashboardFragment.kt).
    if (code === 0) return { icon: "sun", label: "晴" };
    if (code === 1 || code === 2) return { icon: "cloud-sun", label: "多云" };
    if (code === 3) return { icon: "cloud", label: "阴" };
    if (code === 45 || code === 48) return { icon: "cloud-fog", label: "雾" };
    if (code >= 51 && code <= 57) return { icon: "cloud-drizzle", label: "毛毛雨" };
    if (code >= 61 && code <= 67) return { icon: "cloud-rain", label: "雨" };
    if (code >= 71 && code <= 77) return { icon: "cloud-snow", label: "雪" };
    if (code >= 80 && code <= 82) return { icon: "cloud-rain", label: "阵雨" };
    if (code >= 85 && code <= 86) return { icon: "cloud-snow", label: "阵雪" };
    if (code >= 95) return { icon: "cloud-lightning", label: "雷雨" };
    return { icon: "cloud", label: "—" };
}
