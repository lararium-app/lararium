export interface Session {
  id: string;
  created: string;
  kind: string;
  title?: string;
  parent?: string;
  model_pin?: string;
  preview?: string;
  last_message?: string;
}

export interface SurfaceEvent {
  t: "msg" | "tool_call" | "tool_result" | "compact" | "branch";
  seq?: number;
  ts?: string;
  created?: string;
  time?: string;
  role?: "user" | "assistant";
  text?: string;
  error?: string;
  name?: string;
  call_id?: string;
  args?: any;
  ok?: boolean;
  result_digest?: string;
  from_session?: string;
}

export interface Message {
  role: "user" | "assistant";
  text: string;
  created?: string;
  time?: string;
  model?: string;
}

export interface ApprovalRequest {
  approval_id: string;
  name: string;
  args_summary: string;
  session_id?: string;
}

export interface CustosCard {
  id: string;
  cell: string;
  cred: string;
  tool: string;
  dest: string;
  review: string;
  age_s: number;
  expires_in_s: number;
}

export interface CustosGone {
  id: string;
  state: string;
  reason: string;
}

export interface PolicyRule {
  lane: "credential" | "egress";
  pattern: string;
  verdict: "auto" | "ask" | "deny";
  always: boolean;
  source: string;
}

export interface CellStatus {
  id: string;
  name: string;
  state: "running" | "idle" | "stopped" | "error";
  memory_bytes?: number;
  cpu_pct?: number;
  uptime_s?: number;
}

export interface KeyRow {
  name: string;
  status: string;
  sha256_8?: string;
}

export interface FastiHeartbeatState {
  enabled: boolean;
  cadence: string;
  quiet: string;
}

export interface FastiSchedule {
  kind: "cron" | "interval";
  expr?: string;
  every?: string;
}

export interface FastiQuietOverride {
  window: string;
  mode: "defers" | "announce_only";
}

export interface FastiBudgetCounters {
  total_runs?: number;
  total_skips?: number;
  total_defers?: number;
  total_failures?: number;
}

export interface FastiJob {
  id: string;
  name?: string;
  schedule: FastiSchedule;
  prompt: string;
  session?: string;
  announce?: string;
  quiet?: FastiQuietOverride;
  enabled: boolean;
  paused: boolean;
  created_by: string;
  last_fire?: string;
  next_due?: string;
  consecutive_skips: number;
  budget_counters?: FastiBudgetCounters;
}

export interface FastiRequirement {
  text: string;
  done: boolean;
  evidence?: number[];
}

export interface FastiGoal {
  id: string;
  title: string;
  status: "active" | "paused" | "done" | "failed" | "stale";
  requirements: FastiRequirement[];
  turn_count: number;
  turn_budget: number;
  next_due?: string;
  session_id: string;
  next_hint?: string;
  progress_prose?: string;
  created_at: string;
  created_seq?: number;
  updated_at?: string;
}

export interface FastiNoticeStats {
  count: number;
  last_time: string;
}

export interface FastiNoiseSnapshot {
  current_day: string;
  announce_count: number;
  max_announce_per_day: number;
  overflow_day_streak: string[];
  dedup_window_count: number;
  notices?: Record<string, FastiNoticeStats>;
}

export interface FastiStateResponse {
  jobs: FastiJob[];
  goals: FastiGoal[];
  heartbeat: FastiHeartbeatState;
  noise: FastiNoiseSnapshot;
}

export interface FastiEvent {
  kind: "job" | "goal" | "heartbeat";
  op: "upsert" | "delete";
  row: any;
}

