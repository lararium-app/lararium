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

export interface DoorPolicy {
  name: string;
  status: "open" | "closed" | "guarded" | "restricted";
  description?: string;
  allowed_tools?: string[];
  requires_approval?: boolean;
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
