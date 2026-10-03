(() => {
"use strict";

const statusEl = document.getElementById("status");
const sessionsEl = document.getElementById("sessions");
const newSessionBtn = document.getElementById("new-session");
const transcriptEl = document.getElementById("transcript");
const composerEl = document.getElementById("composer");
const inputEl = document.getElementById("input");

let token = null;
let currentSessionId = null;
let catchUpTimer = null;
let isStreaming = false;
let currentAssistantBubble = null;
let currentAssistantText = "";

function showStatus(msg) {
  statusEl.textContent = msg;
}

function clearStatus() {
  statusEl.textContent = "";
}

function setComposerDisabled(disabled) {
  inputEl.disabled = disabled;
  composerEl.querySelector("button").disabled = disabled;
}

function createElement(tag, className, textContent) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (textContent !== undefined) el.textContent = textContent;
  return el;
}

function renderSessionList(sessions) {
  sessionsEl.textContent = "";
  for (const s of sessions) {
    const li = createElement("li");
    const label = createElement("span");
    label.textContent = s.title || s.id;
    if (s.kind === "side") {
      const marker = createElement("span", "kind-marker");
      marker.textContent = " ⟳";
      label.appendChild(marker);
    }
    li.appendChild(label);
    li.dataset.id = s.id;
    if (s.id === currentSessionId) li.classList.add("active");
    li.addEventListener("click", () => selectSession(s.id));
    sessionsEl.appendChild(li);
  }
}

async function fetchJson(method, path, body) {
  const headers = {
    "Content-Type": "application/json",
    "Authorization": "Bearer " + token
  };
  const opts = { method, headers };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const resp = await fetch(path, opts);
  if (resp.status === 401) {
    showStatus("unauthorized — token revoked or wrong");
    throw new Error("unauthorized");
  }
  if (!resp.ok) {
    const err = await resp.json().catch(() => ({ error: resp.statusText }));
    throw new Error(err.error || resp.statusText);
  }
  if (resp.status === 204) return null;
  return resp.json();
}

async function loadSessions() {
  const data = await fetchJson("GET", "/v1/sessions");
  renderSessionList(data);
  if (data.length === 0 && !currentSessionId) {
    // First run: open the door and start typing — no dead composer.
    await createSession();
    return;
  }
  if (data.length > 0 && !currentSessionId) {
    selectSession(data[0].id);
  }
}

async function createSession() {
  const data = await fetchJson("POST", "/v1/sessions", {});
  await loadSessions();
  selectSession(data.id);
}

function selectSession(id) {
  currentSessionId = id;
  for (const li of sessionsEl.children) {
    li.classList.toggle("active", li.dataset.id === id);
  }
  loadTranscript();
}

function renderEvent(ev) {
  // Penatus event schema (PENATUS-SPEC §2, frozen): every event has `t`
  // (type) and `seq`; msg events carry `role`; tool_result carries
  // `result_digest`. Unknown `t` values render as nothing (forward-compat).
  const wrapper = createElement("div", "event");
  if (ev.t === "msg" && ev.role === "user") {
    const bubble = createElement("div", "bubble user");
    bubble.textContent = ev.text;
    wrapper.appendChild(bubble);
  } else if (ev.t === "msg" && ev.role === "assistant") {
    const bubble = createElement("div", "bubble assistant");
    bubble.textContent = ev.text;
    wrapper.appendChild(bubble);
  } else if (ev.t === "tool_call") {
    const details = createElement("details", "tool-call");
    const summary = createElement("summary");
    summary.textContent = "⚙ " + ev.name + " (approval)";
    details.appendChild(summary);
    const content = createElement("div", "tool-content");
    content.dataset.callId = ev.call_id;
    content.textContent = "args: " + (typeof ev.args === "string"
      ? ev.args : JSON.stringify(ev.args || {}));
    details.appendChild(content);
    wrapper.appendChild(details);
  } else if (ev.t === "tool_result") {
    const existing = transcriptEl.querySelector('[data-call-id="' + ev.call_id + '"]');
    if (existing) {
      const outcome = createElement("div", ev.ok ? "tool-outcome" : "tool-outcome err");
      outcome.textContent = "→ " + (ev.ok ? "ok" : "error") + ": " + (ev.result_digest || "");
      existing.appendChild(outcome);
      existing.closest("details").open = true;
    }
  } else if (ev.t === "compact") {
    const sep = createElement("hr", "compact-sep");
    wrapper.appendChild(sep);
  } else if (ev.t === "branch") {
    const note = createElement("div", "branch-note");
    note.textContent = "branched from " + (ev.from_session || "?");
    wrapper.appendChild(note);
  }
  transcriptEl.appendChild(wrapper);
  transcriptEl.scrollTop = transcriptEl.scrollHeight;
}

async function loadTranscript() {
  transcriptEl.textContent = "";
  try {
    const data = await fetchJson("GET", "/v1/sessions/" + currentSessionId + "/events?after_seq=0");
    for (const ev of data.events) {
      renderEvent(ev);
    }
    if (data.in_flight) {
      startCatchUpPoll();
    } else {
      stopCatchUpPoll();
    }
  } catch (e) {
    if (e.message !== "unauthorized") showStatus("failed to load transcript: " + e.message);
  }
}

function startCatchUpPoll() {
  stopCatchUpPoll();
  showStatus("turn in flight…");
  catchUpTimer = setInterval(async () => {
    if (document.hidden) return;
    try {
      const data = await fetchJson("GET", "/v1/sessions/" + currentSessionId + "/events?after_seq=0");
      transcriptEl.textContent = "";
      for (const ev of data.events) {
        renderEvent(ev);
      }
      if (!data.in_flight) {
        stopCatchUpPoll();
        clearStatus();
      }
    } catch (e) {
      if (e.message !== "unauthorized") showStatus("catch-up failed: " + e.message);
      stopCatchUpPoll();
    }
  }, 2000);
}

function stopCatchUpPoll() {
  if (catchUpTimer) {
    clearInterval(catchUpTimer);
    catchUpTimer = null;
  }
}

function handleSSEEvent(eventType, data) {
  if (eventType === "delta") {
    if (!currentAssistantBubble) {
      currentAssistantBubble = createElement("div", "bubble assistant");
      const wrapper = createElement("div", "event");
      wrapper.appendChild(currentAssistantBubble);
      transcriptEl.appendChild(wrapper);
      currentAssistantText = "";
    }
    currentAssistantText += data.text;
    currentAssistantBubble.textContent = currentAssistantText;
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
  } else if (eventType === "tool_start") {
    const details = createElement("details", "tool-call");
    const summary = createElement("summary");
    summary.textContent = "⚙ " + data.name + " (running)";
    details.appendChild(summary);
    const content = createElement("div", "tool-content");
    content.dataset.callId = data.call_id;
    content.textContent = "args: " + data.args_summary;
    details.appendChild(content);
    const wrapper = createElement("div", "event");
    wrapper.appendChild(details);
    transcriptEl.appendChild(wrapper);
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
  } else if (eventType === "approval_request") {
    renderApprovalCard(data);
  } else if (eventType === "tool_done") {
    const content = transcriptEl.querySelector('[data-call-id="' + data.call_id + '"]');
    if (content) {
      const outcome = createElement("div", data.ok ? "tool-outcome" : "tool-outcome err");
      outcome.textContent = "→ " + (data.ok ? "ok" : "error") + (data.approval ? " (approval: " + data.approval + ")" : "");
      content.appendChild(outcome);
      content.closest("details").querySelector("summary").textContent = "⚙ " + content.closest("details").querySelector("summary").textContent.replace("(running)", "(done)");
      content.closest("details").open = true;
    }
  } else if (eventType === "turn_done") {
    isStreaming = false;
    setComposerDisabled(false);
    currentAssistantBubble = null;
    currentAssistantText = "";
    loadTranscript();
  } else if (eventType === "error") {
    isStreaming = false;
    setComposerDisabled(false);
    currentAssistantBubble = null;
    currentAssistantText = "";
    showStatus("error: " + data.error);
  }
}

function renderApprovalCard(data) {
  const wrapper = createElement("div", "event approval-card");
  const msg = createElement("div", "approval-message");
  msg.textContent = "Tool " + data.name + " requests approval:\n" + data.args_summary;
  wrapper.appendChild(msg);
  const btnRow = createElement("div", "approval-buttons");
  const allowBtn = createElement("button", "allow");
  allowBtn.textContent = "Allow";
  const denyBtn = createElement("button", "deny");
  denyBtn.textContent = "Deny";
  let resolved = false;
  async function decide(decision) {
    if (resolved) return;
    resolved = true;
    allowBtn.disabled = true;
    denyBtn.disabled = true;
    try {
      await fetchJson("POST", "/v1/sessions/" + currentSessionId + "/approvals/" + data.approval_id, { decision });
    } catch (e) {
      if (e.message !== "unauthorized") showStatus("approval failed: " + e.message);
    }
    btnRow.remove();
  }
  allowBtn.addEventListener("click", () => decide("allow"));
  denyBtn.addEventListener("click", () => decide("deny"));
  btnRow.appendChild(allowBtn);
  btnRow.appendChild(denyBtn);
  wrapper.appendChild(btnRow);
  transcriptEl.appendChild(wrapper);
  transcriptEl.scrollTop = transcriptEl.scrollHeight;
}

async function sendMessage(text) {
  if (isStreaming) return;
  if (!currentSessionId) {
    // Race or failed load: make sure there is a session before posting.
    try { await createSession(); } catch { showStatus("could not open a session"); setComposerDisabled(false); return; }
  }
  isStreaming = true;
  setComposerDisabled(true);
  clearStatus();
  try {
    const resp = await fetch("/v1/sessions/" + currentSessionId + "/messages", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
        "Authorization": "Bearer " + token
      },
      body: JSON.stringify({ text })
    });
    if (resp.status === 401) {
      showStatus("unauthorized — token revoked or wrong");
      isStreaming = false;
      setComposerDisabled(false);
      return;
    }
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({ error: resp.statusText }));
      showStatus("send failed: " + (err.error || resp.statusText));
      isStreaming = false;
      setComposerDisabled(false);
      return;
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split("\n\n");
        buffer = parts.pop() || "";
        for (const part of parts) {
          if (!part.trim() || part.startsWith(":")) continue;
          let eventType = null;
          let eventData = null;
          for (const line of part.split("\n")) {
            if (line.startsWith("event:")) eventType = line.slice(6).trim();
            else if (line.startsWith("data:")) eventData = JSON.parse(line.slice(5).trim());
          }
          if (eventType && eventData) handleSSEEvent(eventType, eventData);
        }
      }
    } finally {
      // stream closed for any reason (cancel, disconnect, lost terminal
      // event): never leave the composer frozen.
      if (isStreaming) {
        isStreaming = false;
        setComposerDisabled(false);
        currentAssistantBubble = null;
        currentAssistantText = "";
        loadTranscript();
      }
    }
  } catch (e) {
    if (e.message !== "unauthorized") showStatus("stream error: " + e.message);
    isStreaming = false;
    setComposerDisabled(false);
  }
}

function initToken() {
  if (location.hash.startsWith("#lar1_")) {
    token = location.hash.slice(1);
    sessionStorage.setItem("lar_token", token);
    history.replaceState(null, "", location.pathname);
  } else {
    token = sessionStorage.getItem("lar_token");
  }
  if (!token) {
    showStatus("open the URL printed by hearthd token create");
    return false;
  }
  return true;
}

function init() {
  if (!initToken()) return;
  newSessionBtn.addEventListener("click", createSession);
  composerEl.addEventListener("submit", (e) => {
    e.preventDefault();
    const text = inputEl.value.trim();
    if (!text) return;
    inputEl.value = "";
    sendMessage(text);
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) stopCatchUpPoll();
    else if (currentSessionId) loadTranscript();
  });
  loadSessions();
}

document.addEventListener("DOMContentLoaded", init);
})();