// Thin fetch client for the audit console API. Everything the UI shows comes
// from these calls, which also means the same UI works against a remote server.

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

const TOKEN_KEY = "argus.token";

export function token(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(value: string): void {
  localStorage.setItem(TOKEN_KEY, value);
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers ?? {});
  if (init?.body) headers.set("Content-Type", "application/json");
  const bearer = token();
  if (bearer) headers.set("Authorization", `Bearer ${bearer}`);
  const response = await fetch(path, { ...init, headers });
  if (!response.ok) {
    const text = await response.text();
    throw new ApiError(text.trim() || response.statusText, response.status);
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  roots: () => request<string[]>("/api/roots"),
  runs: (root: string) => request<unknown[]>(`/api/runs?root=${encodeURIComponent(root)}`),
  run: (path: string) => request<unknown>(`/api/run?path=${encodeURIComponent(path)}`),
  labels: (path: string) => request<Record<string, string>>(`/api/labels?path=${encodeURIComponent(path)}`),
  label: (body: unknown) => request<unknown>("/api/label", { method: "POST", body: JSON.stringify(body) }),
  calibration: () => request<unknown>("/api/calibration"),
  recalibrate: () => request<unknown>("/api/calibration/rebuild", { method: "POST" }),
  launched: () => request<unknown[]>("/api/launched"),
  launch: (body: unknown) => request<unknown>("/api/launch", { method: "POST", body: JSON.stringify(body) }),
  stopLaunch: (id: string) => request<unknown>("/api/launch/stop", { method: "POST", body: JSON.stringify({ id }) }),
};

/** evidenceUrl prefixes a relative artefact path with the run id. */
export function evidenceUrl(path: string): string {
  if (!path) return "";
  if (path.startsWith("http") || path.startsWith("/evidence/")) return path;
  return path;
}
