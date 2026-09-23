import client, { getToken } from "./client";

export interface VisionStatus {
    online: boolean;
    face_engine_ready?: boolean;
    pose_engine_ready?: boolean;
    registered_persons?: number;
    cpu_usage_percent?: number;
    cpu_gate?: "normal" | "degraded" | "circuit_break";
    error?: string;
}

export interface VisionPerson {
    name: string;
}

/** Get Vision AI service status, model readiness and CPU gating */
export async function getVisionStatus(): Promise<VisionStatus> {
    const { data } = await client.get<VisionStatus>("/vision/status");
    return data;
}

/** List all registered family persons */
export async function listPersons(): Promise<VisionPerson[]> {
    const { data } = await client.get<VisionPerson[]>("/vision/persons");
    return data ?? [];
}

/** Register a new person with Base64 image data */
export async function registerPerson(name: string, imageBase64: string): Promise<{ message: string }> {
    const { data } = await client.post<{ message: string }>("/vision/persons", {
        name,
        image: imageBase64,
    });
    return data;
}

/** Delete a registered person */
export async function deletePerson(name: string): Promise<{ message: string }> {
    const { data } = await client.delete<{ message: string }>(`/vision/persons/${encodeURIComponent(name)}`);
    return data;
}

/** Helper: Fetch image from snapshot URL, convert to Base64 and register */
export async function registerPersonFromSnapshot(name: string, snapshotUrl: string): Promise<{ message: string }> {
    const token = getToken();
    const headers: Record<string, string> = {};
    if (token) {
        headers["Authorization"] = `Bearer ${token}`;
    }

    const res = await fetch(snapshotUrl, { headers });
    if (!res.ok) {
        throw new Error(`无法获取快照图像 (HTTP ${res.status})`);
    }

    const blob = await res.blob();
    const base64 = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onloadend = () => {
            const dataUrl = reader.result as string;
            // Strip data:image/...;base64, prefix
            const b64 = dataUrl.includes(",") ? dataUrl.split(",")[1] : dataUrl;
            resolve(b64);
        };
        reader.onerror = reject;
        reader.readAsDataURL(blob);
    });

    return registerPerson(name, base64);
}
