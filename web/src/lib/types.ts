// Shared types mirroring the Go service payloads.

export interface RunSummary {
  path: string;
  id: string;
  subject: string;
  version?: string;
  mode: string;
  provider: string;
  decisionModel: string;
  visionModel?: string;
  startedAt: string;
  durationMs: number;
  terminal: string;
  reported: number;
  total: number;
  rejected: number;
  costUsd: number;
  severities: Record<string, number>;
}

export interface Region {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
}

export interface Finding {
  id: string;
  title: string;
  brief: string;
  group: string;
  screen: string;
  feature?: string;
  features?: string[];
  region: Region;
  check?: string;
  class: string;
  severity: number;
  severity_score: number;
  severity_name: string;
  defect_prob: number;
  confidence: number;
  intentional: boolean;
  reported: boolean;
  verified: string;
  detail?: string;
  measured?: number;
  events: number;
  first_seen_ms: number;
  last_seen_ms: number;
  grid_id: number;
  model_answers?: Record<string, number>;
  tags?: string[];
}

export interface EvidenceSet {
  still?: string;
  crop?: string;
  strip?: string;
  clip?: string;
}

export interface RunFinding extends Finding {
  evidence: EvidenceSet;
}

export interface CandidateRow {
  observation: string;
  screen: string;
  action: string;
  check: string;
  region: string;
  regionXY: [number, number, number, number];
  detail: string;
  severityHint: number;
  verdict: string;
}

export interface RunDetail {
  summary: RunSummary;
  findings: RunFinding[];
  rejected: RunFinding[];
  candidates: CandidateRow[];
  sessionClip?: string;
  isolation?: Record<string, unknown>;
  notes: string[];
  log: string[];
  observations: number;
  frameCount: number;
  decisionCalls: number;
  visionCalls: number;
}

export const SEVERITY_ORDER = ["critical", "major", "noticeable", "minor", "cosmetic"];

export const SEVERITY_COLOURS: Record<string, string> = {
  critical: "#ff4d4d",
  major: "#ff9f1c",
  noticeable: "#ffd60a",
  minor: "#6c9aff",
  cosmetic: "#7c8598",
};
