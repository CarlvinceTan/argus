// Small helpers shared by the views.

import type { Finding, Region } from "./types";
import { SEVERITY_ORDER } from "./types";

export function severityRank(name: string): number {
  const index = SEVERITY_ORDER.indexOf(name);
  return index < 0 ? SEVERITY_ORDER.length : index;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)} s`;
  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds % 60);
  return `${minutes}m ${rest}s`;
}

export function formatClock(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return date.toLocaleString();
}

export function formatCost(value: number): string {
  if (!value) return "free";
  return `$${value.toFixed(5)}`;
}

export function groupKey(finding: Finding, mode: "page" | "feature"): string {
  if (mode === "feature") {
    return finding.feature || finding.group || "unlabelled";
  }
  return finding.group || "unlabelled";
}

export function groupFindings(
  findings: Finding[],
  mode: "page" | "feature",
): { key: string; findings: Finding[]; worst: string }[] {
  const buckets = new Map<string, Finding[]>();
  for (const finding of findings) {
    const key = groupKey(finding, mode);
    const bucket = buckets.get(key) ?? [];
    bucket.push(finding);
    buckets.set(key, bucket);
  }
  return [...buckets.entries()]
    .map(([key, bucket]) => {
      const sorted = [...bucket].sort(
        (a, b) => severityRank(a.severity_name) - severityRank(b.severity_name) || b.defect_prob - a.defect_prob,
      );
      return { key, findings: sorted, worst: sorted[0]?.severity_name ?? "cosmetic" };
    })
    .sort((a, b) => severityRank(a.worst) - severityRank(b.worst) || a.key.localeCompare(b.key));
}

/** Region as percentages of the rendered still, for the overlay box. */
export function regionStyle(region: Region, terminal: string): string {
  const [cols, rows] = terminal.split("x").map((value) => Number(value) || 1);
  const left = (region.x0 / cols) * 100;
  const top = (region.y0 / rows) * 100;
  const width = ((region.x1 - region.x0 + 1) / cols) * 100;
  const height = ((region.y1 - region.y0 + 1) / rows) * 100;
  return `left:${left}%;top:${top}%;width:${width}%;height:${height}%;`;
}

/** Region as cell coordinates, for the textual readout. */
export function regionLabel(region: Region): string {
  return `x ${region.x0}–${region.x1}, y ${region.y0}–${region.y1}`;
}

export function modelAnswers(finding: Finding): { key: string; value: number }[] {
  return Object.entries(finding.model_answers ?? {})
    .map(([key, value]) => ({ key, value }))
    .sort((a, b) => b.value - a.value);
}
