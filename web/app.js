(() => {
"use strict";

const statusEl = document.getElementById("status");
const sessionsEl = document.getElementById("sessions");
const newSessionBtn = document.getElementById("new-session");
const transcriptEl = document.getElementById("transcript");
const custosCardsEl = document.getElementById("custos-cards");
const composerEl = document.getElementById("composer");
const inputEl = document.getElementById("input");
const topbarTitleEl = document.getElementById("topbar-title");
const topbarModelEl = document.getElementById("topbar-model");
const topbarModelNameEl = document.getElementById("topbar-model-name");
const topbarTransportEl = document.getElementById("topbar-transport");
const sessionSearchEl = document.getElementById("session-search");
const navToggleBtn = document.getElementById("nav-toggle");
const railBackdropEl = document.getElementById("rail-backdrop");
const themeBtnEl = document.getElementById("theme-btn");
const themePopoverEl = document.getElementById("theme-popover");
const accentHueEl = document.getElementById("accent-hue");
const hueValEl = document.getElementById("hue-val");
const shareBtnEl = document.getElementById("share-btn");

let token = null;
let currentSessionId = null;
let currentSessionModel = null;
let catchUpTimer = null;
let isStreaming = false;
let currentAssistantBubble = null;
let currentAssistantText = "";
let loadedSessions = [];
let loadedEvents = [];
let lastUserMessageText = "";

// --- Theme System ---

function initTheme() {
  const savedTheme = sessionStorage.getItem("lararium.theme") || "warm";
  const hue = sessionStorage.getItem("lararium.accent-hue") || "25";
  const base = sessionStorage.getItem("lararium.base") || "dark";

  document.documentElement.dataset.theme = savedTheme;
  document.documentElement.style.setProperty("--accent-h", hue);
  document.documentElement.dataset.base = base;

  if (accentHueEl) accentHueEl.value = hue;
  if (hueValEl) hueValEl.textContent = hue;
  updateThemeUI();
}

function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  sessionStorage.setItem("lararium.theme", theme);
  if (theme === "custom") {
    const hue = sessionStorage.getItem("lararium.accent-hue") || "25";
    document.documentElement.style.setProperty("--accent-h", hue);
    const base = sessionStorage.getItem("lararium.base") || "dark";
    document.documentElement.dataset.base = base;
  }
  updateThemeUI();
}

function setBase(base) {
  document.documentElement.dataset.base = base;
  sessionStorage.setItem("lararium.base", base);
  updateThemeUI();
}

function updateThemeUI() {
  const curTheme = document.documentElement.dataset.theme || "warm";
  const curBase = document.documentElement.dataset.base || "dark";

  if (themePopoverEl) {
    for (const btn of themePopoverEl.querySelectorAll(".theme-opt")) {
      btn.classList.toggle("active", btn.dataset.themeVal === curTheme);
    }
    const customControls = document.getElementById("custom-theme-controls");
    if (customControls) {
      customControls.hidden = curTheme !== "custom";
    }
    for (const btn of themePopoverEl.querySelectorAll(".base-btn")) {
      btn.classList.toggle("active", btn.dataset.baseVal === curBase);
    }
  }
}

function toggleThemePopover() {
  if (!themePopoverEl) return;
  themePopoverEl.hidden = !themePopoverEl.hidden;
  if (!themePopoverEl.hidden) {
    updateThemeUI();
  }
}

function closeThemePopover() {
  if (themePopoverEl) themePopoverEl.hidden = true;
}

// --- Helpers ---

function showStatus(msg) {
  if (statusEl) statusEl.textContent = msg;
  const dot = document.getElementById("status-dot");
  if (dot) {
    if (msg.includes("error") || msg.includes("failed") || msg.includes("unauthorized")) {
      dot.className = "dot err";
    } else {
      dot.className = "dot warn";
    }
  }
}

function clearStatus() {
  if (statusEl) statusEl.textContent = "hearth online";
  const dot = document.getElementById("status-dot");
  if (dot) dot.className = "dot";
}

function setComposerDisabled(disabled) {
  inputEl.disabled = disabled;
  const sendBtn = composerEl.querySelector('button[type="submit"]');
  if (sendBtn) sendBtn.disabled = disabled;
  const stopBtn = document.getElementById("stop-btn");
  if (stopBtn) stopBtn.disabled = !disabled;
}

function createElement(tag, className, textContent) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (textContent !== undefined) el.textContent = textContent;
  return el;
}

function formatTime(val) {
  try {
    const d = val ? new Date(val) : new Date();
    if (!isNaN(d.getTime())) {
      return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    }
  } catch (_) {}
  return "";
}

function autoGrowTextarea() {
  inputEl.style.setProperty("height", "auto");
  const nextHeight = Math.min(inputEl.scrollHeight, 200);
  inputEl.style.setProperty("height", nextHeight + "px");
}

function toggleRail() {
  document.body.classList.toggle("rail-open");
}

function closeRail() {
  document.body.classList.remove("rail-open");
}

// --- Hand-rolled XSS-safe Markdown Renderer ---

function parseMarkdownBlocks(text) {
  const lines = text.split(/\r?\n/);
  const blocks = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block (```lang)
    const fenceMatch = line.match(/^```(\w*)/);
    if (fenceMatch) {
      const lang = fenceMatch[1] || "";
      i++;
      const codeLines = [];
      let closed = false;
      while (i < lines.length) {
        if (/^```\s*$/.test(lines[i])) {
          closed = true;
          i++;
          break;
        }
        codeLines.push(lines[i]);
        i++;
      }
      blocks.push({
        type: "code",
        lang: lang,
        content: codeLines.join("\n"),
        closed: closed
      });
      continue;
    }

    // Heading (#, ##, ###)
    const headingMatch = line.match(/^(#{1,3})\s+(.*)$/);
    if (headingMatch) {
      blocks.push({
        type: "heading",
        level: headingMatch[1].length,
        text: headingMatch[2]
      });
      i++;
      continue;
    }

    // Horizontal rule (---, ***, ___)
    if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(line)) {
      blocks.push({ type: "hr" });
      i++;
      continue;
    }

    // Unordered list (- item, * item)
    if (/^[-*]\s+(.*)$/.test(line)) {
      const items = [];
      while (i < lines.length) {
        const itemMatch = lines[i].match(/^[-*]\s+(.*)$/);
        if (itemMatch) {
          items.push(itemMatch[1]);
          i++;
        } else if (lines[i].trim() === "") {
          break;
        } else {
          if (items.length > 0 && !/^(\d+\.|#{1,3}|```|[-*]\s+)/.test(lines[i])) {
            items[items.length - 1] += " " + lines[i].trim();
            i++;
          } else {
            break;
          }
        }
      }
      blocks.push({ type: "ul", items });
      continue;
    }

    // Ordered list (1. item)
    if (/^\d+\.\s+(.*)$/.test(line)) {
      const items = [];
      while (i < lines.length) {
        const itemMatch = lines[i].match(/^\d+\.\s+(.*)$/);
        if (itemMatch) {
          items.push(itemMatch[1]);
          i++;
        } else if (lines[i].trim() === "") {
          break;
        } else {
          if (items.length > 0 && !/^(\d+\.|#{1,3}|```|[-*]\s+)/.test(lines[i])) {
            items[items.length - 1] += " " + lines[i].trim();
            i++;
          } else {
            break;
          }
        }
      }
      blocks.push({ type: "ol", items });
      continue;
    }

    // Blank lines
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Paragraph
    const paraLines = [];
    while (i < lines.length) {
      const cur = lines[i];
      if (cur.trim() === "") break;
      if (/^```/.test(cur)) break;
      if (/^(#{1,3})\s+/.test(cur)) break;
      if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(cur)) break;
      if (/^[-*]\s+/.test(cur)) break;
      if (/^\d+\.\s+/.test(cur)) break;
      paraLines.push(cur);
      i++;
    }
    if (paraLines.length > 0) {
      blocks.push({ type: "p", lines: paraLines });
    }
  }

  return blocks;
}

function renderInline(text, parentEl) {
  function parseLinkAt(str, start) {
    const prefixMatch = str.slice(start).match(/^\[([^\]]+)\]\(/);
    if (!prefixMatch) return null;
    let depth = 1;
    let i = start + prefixMatch[0].length;
    while (i < str.length && depth > 0) {
      if (str[i] === '(') depth++;
      else if (str[i] === ')') depth--;
      i++;
    }
    if (depth === 0) {
      const url = str.slice(start + prefixMatch[0].length, i - 1);
      return {
        linkText: prefixMatch[1],
        url: url.trim(),
        totalLen: i - start
      };
    }
    return null;
  }

  let pos = 0;
  while (pos < text.length) {
    // Inline code `...`
    if (text[pos] === '`') {
      const endTick = text.indexOf('`', pos + 1);
      if (endTick !== -1) {
        const codeText = text.slice(pos + 1, endTick);
        const codeEl = createElement("code", "inline", codeText);
        parentEl.appendChild(codeEl);
        pos = endTick + 1;
        continue;
      }
    }

    // Bold **...**
    if (text.startsWith('**', pos)) {
      const endBold = text.indexOf('**', pos + 2);
      if (endBold !== -1) {
        const boldText = text.slice(pos + 2, endBold);
        const bEl = createElement("b");
        renderInline(boldText, bEl);
        parentEl.appendChild(bEl);
        pos = endBold + 2;
        continue;
      }
    }

    // Italic *...*
    if (text[pos] === '*' && text[pos + 1] !== '*' && text[pos + 1] !== ' ') {
      const endItalic = text.indexOf('*', pos + 1);
      if (endItalic !== -1 && endItalic > pos + 1) {
        const italicText = text.slice(pos + 1, endItalic);
        const iEl = createElement("i");
        renderInline(italicText, iEl);
        parentEl.appendChild(iEl);
        pos = endItalic + 1;
        continue;
      }
    }

    // Link [text](url)
    if (text[pos] === '[') {
      const linkInfo = parseLinkAt(text, pos);
      if (linkInfo) {
        const isSafe = /^https?:\/\//i.test(linkInfo.url) || /^mailto:/i.test(linkInfo.url);
        if (isSafe) {
          const a = createElement("a");
          a.href = linkInfo.url;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          a.textContent = linkInfo.linkText;
          parentEl.appendChild(a);
        } else {
          // Drop unsafe url protocol (e.g. javascript:)
          parentEl.appendChild(document.createTextNode(linkInfo.linkText));
        }
        pos += linkInfo.totalLen;
        continue;
      }
    }

    // Plain text accumulation
    let nextSpecial = pos + 1;
    while (nextSpecial < text.length) {
      const ch = text[nextSpecial];
      if (ch === '`' || ch === '*' || ch === '[') break;
      nextSpecial++;
    }
    const plainChunk = text.slice(pos, nextSpecial);
    parentEl.appendChild(document.createTextNode(plainChunk));
    pos = nextSpecial;
  }
}

function renderMarkdown(text, containerEl) {
  containerEl.replaceChildren();
  const blocks = parseMarkdownBlocks(text);

  for (const block of blocks) {
    if (block.type === "code") {
      const blockEl = createElement("div", "codeblock");
      const headEl = createElement("div", "head");
      const langLabel = createElement("span", "lang", block.lang || "code");
      headEl.appendChild(langLabel);

      const copyBtn = createElement("button", "copy", "Copy");
      copyBtn.type = "button";
      copyBtn.title = "Copy code";
      copyBtn.addEventListener("click", () => {
        navigator.clipboard.writeText(block.content).then(() => {
          copyBtn.textContent = "Copied!";
          setTimeout(() => { copyBtn.textContent = "Copy"; }, 1500);
        }).catch(() => {});
      });
      headEl.appendChild(copyBtn);
      blockEl.appendChild(headEl);

      const preEl = createElement("pre");
      const codeEl = createElement("code", "", block.content);
      preEl.appendChild(codeEl);
      blockEl.appendChild(preEl);
      containerEl.appendChild(blockEl);
    } else if (block.type === "heading") {
      const tag = block.level === 1 ? "h3" : (block.level === 2 ? "h4" : "h5");
      const hEl = createElement(tag, tag);
      renderInline(block.text, hEl);
      containerEl.appendChild(hEl);
    } else if (block.type === "hr") {
      containerEl.appendChild(createElement("hr"));
    } else if (block.type === "ul") {
      const ulEl = createElement("ul", "bullets");
      for (const itemText of block.items) {
        const liEl = createElement("li");
        renderInline(itemText, liEl);
        ulEl.appendChild(liEl);
      }
      containerEl.appendChild(ulEl);
    } else if (block.type === "ol") {
      const olEl = createElement("ol", "ordered-list");
      for (const itemText of block.items) {
        const liEl = createElement("li");
        renderInline(itemText, liEl);
        olEl.appendChild(liEl);
      }
      containerEl.appendChild(olEl);
    } else if (block.type === "p") {
      const pEl = createElement("p");
      for (let li = 0; li < block.lines.length; li++) {
        renderInline(block.lines[li], pEl);
        if (li < block.lines.length - 1) {
          pEl.appendChild(createElement("br"));
        }
      }
      containerEl.appendChild(pEl);
    }
  }
}

// --- Session List & Grouping ---

function getSessionGroup(createdStr) {
  if (!createdStr) return "Older";
  const date = new Date(createdStr);
  if (isNaN(date.getTime())) return "Older";
  const now = new Date();

  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const yesterdayStart = todayStart - 86400000;
  const prev7Start = todayStart - 7 * 86400000;

  const t = date.getTime();
  if (t >= todayStart) return "Today";
  if (t >= yesterdayStart) return "Yesterday";
  if (t >= prev7Start) return "Previous 7 days";
  return "Older";
}

function renderSessionList(sessions) {
  sessionsEl.replaceChildren();
  const query = sessionSearchEl ? sessionSearchEl.value.trim().toLowerCase() : "";
  const filtered = query ? sessions.filter(s => (s.title || s.id).toLowerCase().includes(query)) : sessions;

  const groups = { "Today": [], "Yesterday": [], "Previous 7 days": [], "Older": [] };
  let hasTimestamps = false;

  for (const s of filtered) {
    if (s.created) hasTimestamps = true;
    const grp = getSessionGroup(s.created);
    groups[grp].push(s);
  }

  const groupKeys = ["Today", "Yesterday", "Previous 7 days", "Older"];

  for (const gKey of groupKeys) {
    const list = groups[gKey];
    if (list.length === 0) continue;

    if (hasTimestamps) {
      const groupHeader = createElement("div", "group", gKey);
      sessionsEl.appendChild(groupHeader);
    }

    for (const s of list) {
      const li = createElement("li", "sess");
      li.dataset.id = s.id;
      if (s.id === currentSessionId) li.classList.add("active");

      const titleSpan = createElement("span", "t");
      titleSpan.textContent = s.title || s.id;
      if (s.kind === "side" || s.parent) {
        const marker = createElement("span", "badge kind-marker", "branched");
        titleSpan.appendChild(marker);
      }
      li.appendChild(titleSpan);

      const previewText = s.preview || s.last_message;
      if (previewText) {
        const pSpan = createElement("span", "p", previewText);
        li.appendChild(pSpan);
      }

      li.addEventListener("click", () => {
        selectSession(s.id);
        if (window.innerWidth <= 720) closeRail();
      });
      sessionsEl.appendChild(li);
    }
  }
}

// --- API Calls ---

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
  try {
    const data = await fetchJson("GET", "/v1/sessions");
    loadedSessions = data;
    renderSessionList(data);
    if (data.length === 0 && !currentSessionId) {
      await createSession();
      return;
    }
    if (data.length > 0 && !currentSessionId) {
      selectSession(data[0].id);
    }
  } catch (e) {
    if (e.message !== "unauthorized") showStatus("failed to load sessions: " + e.message);
  }
}

async function createSession() {
  try {
    const data = await fetchJson("POST", "/v1/sessions", {});
    await loadSessions();
    selectSession(data.id);
  } catch (e) {
    if (e.message !== "unauthorized") showStatus("failed to create session: " + e.message);
  }
}

function selectSession(id) {
  currentSessionId = id;
  const sessObj = loadedSessions.find(s => s.id === id);
  if (sessObj) {
    currentSessionModel = sessObj.model_pin || null;
    if (topbarTitleEl) topbarTitleEl.textContent = sessObj.title || sessObj.id;
    if (topbarModelEl) {
      if (currentSessionModel) {
        topbarModelEl.hidden = false;
        if (topbarModelNameEl) topbarModelNameEl.textContent = currentSessionModel;
      } else {
        topbarModelEl.hidden = true;
      }
    }
  } else {
    currentSessionModel = null;
    if (topbarTitleEl) topbarTitleEl.textContent = id;
    if (topbarModelEl) topbarModelEl.hidden = true;
  }

  for (const li of sessionsEl.children) {
    if (li.dataset.id) {
      li.classList.toggle("active", li.dataset.id === id);
    }
  }
  loadTranscript();
}

// --- Empty State ---

function renderEmptyState() {
  transcriptEl.replaceChildren();
  const emptyStateEl = createElement("div", "empty-state");
  emptyStateEl.id = "empty-state";

  const flameEl = createElement("div", "flame-large", "🔥");
  emptyStateEl.appendChild(flameEl);

  const titleEl = createElement("h2", "", "Your hearth is warm.");
  emptyStateEl.appendChild(titleEl);

  const pEl = createElement("p", "", "Choose a suggestion or ask anything to begin.");
  emptyStateEl.appendChild(pEl);

  const gridEl = createElement("div", "suggestion-grid");
  const suggestions = [
    "Summarize today's notifications",
    "What did the door sensors see?",
    "Draft a weekly digest",
    "What are my provider keys doing?"
  ];

  for (const sug of suggestions) {
    const chipBtn = createElement("button", "suggestion-chip", sug);
    chipBtn.type = "button";
    chipBtn.addEventListener("click", () => {
      inputEl.value = sug;
      inputEl.focus();
      autoGrowTextarea();
    });
    gridEl.appendChild(chipBtn);
  }

  emptyStateEl.appendChild(gridEl);
  transcriptEl.appendChild(emptyStateEl);
}

function removeEmptyState() {
  const existing = document.getElementById("empty-state");
  if (existing) existing.remove();
}

// --- Transcript Rendering ---

function renderEvent(ev) {
  removeEmptyState();
  const wrapper = createElement("div", "event");

  if (ev.t === "msg" && ev.role === "user") {
    lastUserMessageText = ev.text;
    wrapper.classList.add("msg", "user");
    const mcol = createElement("div", "mcol");
    const bubble = createElement("div", "bubble", ev.text);
    mcol.appendChild(bubble);
    wrapper.appendChild(mcol);
    const avatar = createElement("div", "avatar you", "YOU");
    wrapper.appendChild(avatar);
  } else if (ev.t === "msg" && ev.role === "assistant") {
    wrapper.classList.add("msg", "assistant");
    const avatar = createElement("div", "avatar ember", "🔥");
    wrapper.appendChild(avatar);

    const mcol = createElement("div", "mcol");
    const meta = createElement("div", "meta");
    const author = createElement("b", "", "Hearth");
    meta.appendChild(author);

    const timeStr = formatTime(ev.created || ev.time);
    if (timeStr) {
      meta.appendChild(document.createTextNode(" " + timeStr));
    }
    if (currentSessionModel) {
      meta.appendChild(document.createTextNode(" · " + currentSessionModel));
    }
    mcol.appendChild(meta);

    const bubble = createElement("div", "bubble");
    renderMarkdown(ev.text, bubble);
    mcol.appendChild(bubble);

    const actions = createElement("div", "msg-actions");
    const copyBtn = createElement("button", "", "⧉ Copy");
    copyBtn.type = "button";
    copyBtn.title = "Copy message";
    copyBtn.addEventListener("click", () => {
      navigator.clipboard.writeText(ev.text).then(() => {
        copyBtn.textContent = "Copied!";
        setTimeout(() => { copyBtn.textContent = "⧉ Copy"; }, 1500);
      }).catch(() => {});
    });
    actions.appendChild(copyBtn);

    const retryBtn = createElement("button", "", "↻ Retry");
    retryBtn.type = "button";
    retryBtn.title = "Retry prompt";
    retryBtn.addEventListener("click", () => {
      // Find previous user prompt
      const idx = loadedEvents.indexOf(ev);
      let promptToResend = lastUserMessageText;
      if (idx > 0) {
        for (let j = idx - 1; j >= 0; j--) {
          if (loadedEvents[j].t === "msg" && loadedEvents[j].role === "user") {
            promptToResend = loadedEvents[j].text;
            break;
          }
        }
      }
      if (promptToResend) {
        sendMessage(promptToResend);
      }
    });
    actions.appendChild(retryBtn);

    mcol.appendChild(actions);
    wrapper.appendChild(mcol);
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
      const parentDetails = existing.closest("details");
      if (parentDetails) parentDetails.open = true;
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
  transcriptEl.replaceChildren();
  try {
    const data = await fetchJson("GET", "/v1/sessions/" + currentSessionId + "/events?after_seq=0");
    loadedEvents = data.events || [];
    if (loadedEvents.length === 0) {
      renderEmptyState();
    } else {
      for (const ev of loadedEvents) {
        renderEvent(ev);
      }
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
      loadedEvents = data.events || [];
      transcriptEl.replaceChildren();
      if (loadedEvents.length === 0) {
        renderEmptyState();
      } else {
        for (const ev of loadedEvents) {
          renderEvent(ev);
        }
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
    removeEmptyState();
    if (!currentAssistantBubble) {
      const wrapper = createElement("div", "event msg assistant");
      const avatar = createElement("div", "avatar ember", "🔥");
      wrapper.appendChild(avatar);

      const mcol = createElement("div", "mcol");
      const meta = createElement("div", "meta");
      const author = createElement("b", "", "Hearth");
      meta.appendChild(author);
      meta.appendChild(document.createTextNode(" " + formatTime(new Date())));
      if (currentSessionModel) {
        meta.appendChild(document.createTextNode(" · " + currentSessionModel));
      }
      mcol.appendChild(meta);

      currentAssistantBubble = createElement("div", "bubble");
      mcol.appendChild(currentAssistantBubble);

      wrapper.appendChild(mcol);
      transcriptEl.appendChild(wrapper);
      currentAssistantText = "";
    }
    currentAssistantText += data.text;
    renderMarkdown(currentAssistantText, currentAssistantBubble);
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
  } else if (eventType === "tool_start") {
    removeEmptyState();
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
      const parentDetails = content.closest("details");
      if (parentDetails) {
        const summary = parentDetails.querySelector("summary");
        if (summary) summary.textContent = summary.textContent.replace("(running)", "(done)");
        parentDetails.open = true;
      }
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
  removeEmptyState();
  const wrapper = createElement("div", "event approval-card");
  const msg = createElement("div", "approval-message");
  msg.textContent = "Tool " + data.name + " requests approval:\n" + data.args_summary;
  wrapper.appendChild(msg);
  const btnRow = createElement("div", "approval-buttons");
  const allowBtn = createElement("button", "allow", "Allow");
  allowBtn.type = "button";
  const denyBtn = createElement("button", "deny", "Deny");
  denyBtn.type = "button";
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
    try {
      await createSession();
    } catch {
      showStatus("could not open a session");
      setComposerDisabled(false);
      return;
    }
  }
  lastUserMessageText = text;
  isStreaming = true;
  setComposerDisabled(true);
  clearStatus();
  const stopBtn = document.getElementById("stop-btn");
  if (stopBtn) stopBtn.disabled = false;

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
      if (isStreaming) {
        isStreaming = false;
        setComposerDisabled(false);
        currentAssistantBubble = null;
        currentAssistantText = "";
        loadTranscript();
      }
      if (stopBtn) stopBtn.disabled = true;
    }
  } catch (e) {
    if (e.message !== "unauthorized") showStatus("stream error: " + e.message);
    isStreaming = false;
    setComposerDisabled(false);
    if (stopBtn) stopBtn.disabled = true;
  }
}

async function handleStop() {
  if (!currentSessionId || !isStreaming) return;
  try {
    await fetch("/v1/sessions/" + currentSessionId + "/cancel", {
      method: "POST",
      headers: {
        "Authorization": "Bearer " + token
      }
    });
  } catch (_) {}
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

// --- Provider Keys Drawer ---

const keysButtonEl = document.getElementById("keys-button");
const keysDrawerEl = document.getElementById("keys-drawer");
const keysBackdropEl = document.getElementById("keys-backdrop");
const keysCloseEl = document.getElementById("keys-close");
const keysListEl = document.getElementById("keys-list");
const keysFormEl = document.getElementById("keys-form");
const keysNameEl = document.getElementById("keys-name");
const keysValueEl = document.getElementById("keys-value");
const keysStatusEl = document.getElementById("keys-status");
const keysTransportWarnEl = document.getElementById("keys-transport-warn");

function keysStatus(msg, isError) {
  if (!keysStatusEl) return;
  keysStatusEl.textContent = msg || "";
  keysStatusEl.classList.toggle("err", !!isError);
}

function openKeysDrawer() {
  if (!keysDrawerEl || !keysBackdropEl) return;
  keysDrawerEl.hidden = false;
  keysBackdropEl.hidden = false;

  const host = location.hostname;
  const loopback = host === "localhost" || host === "127.0.0.1" ||
    host === "[::1]" || host === "::1" || host === "";
  if (keysTransportWarnEl) {
    keysTransportWarnEl.hidden = location.protocol === "https:" || loopback;
  }
  loadKeys();
}

function closeKeysDrawer() {
  if (keysDrawerEl) keysDrawerEl.hidden = true;
  if (keysBackdropEl) keysBackdropEl.hidden = true;
  if (keysValueEl) keysValueEl.value = "";
  keysStatus("");
}

async function loadKeys() {
  try {
    const rows = await fetchJson("GET", "/v1/keys");
    renderKeys(rows);
  } catch (e) {
    if (e.message !== "unauthorized") keysStatus("failed to load keys: " + e.message, true);
  }
}

function renderKeys(rows) {
  if (!keysListEl) return;
  keysListEl.replaceChildren();
  for (const row of rows) {
    const li = createElement("li");
    const name = createElement("span", "key-name", row.name);
    const status = createElement("span", "key-status",
      row.status === "missing" ? "missing" : row.status + " · " + row.sha256_8);
    li.appendChild(name);
    li.appendChild(status);
    if (row.status === "set (keys.json)") {
      const del = createElement("button", "key-remove", "remove");
      del.type = "button";
      del.title = "Remove this key from the daemon";
      del.addEventListener("click", () => removeKey(row.name));
      li.appendChild(del);
    }
    keysListEl.appendChild(li);
  }
}

async function saveKey(name, value) {
  try {
    await fetchJson("PUT", "/v1/keys/" + encodeURIComponent(name), { key: value });
    if (keysValueEl) keysValueEl.value = "";
    keysStatus("saved " + name);
    await loadKeys();
  } catch (e) {
    if (e.message === "unknown provider") {
      if (window.confirm('"' + name + '" is not a configured provider. Save it anyway?')) {
        try {
          await fetchJson("PUT", "/v1/keys/" + encodeURIComponent(name) + "?force=true", { key: value });
          if (keysValueEl) keysValueEl.value = "";
          keysStatus("saved " + name);
          await loadKeys();
        } catch (e2) {
          if (e2.message !== "unauthorized") keysStatus(e2.message, true);
        }
      }
      return;
    }
    if (e.message !== "unauthorized") keysStatus(e.message, true);
  }
}

async function removeKey(name) {
  if (!window.confirm("Remove the key for " + name + "?")) return;
  try {
    await fetchJson("DELETE", "/v1/keys/" + encodeURIComponent(name));
    keysStatus("removed " + name);
    await loadKeys();
  } catch (e) {
    if (e.message !== "unauthorized") keysStatus(e.message, true);
  }
}

function initKeysDrawer() {
  if (keysButtonEl) keysButtonEl.addEventListener("click", openKeysDrawer);
  if (keysCloseEl) keysCloseEl.addEventListener("click", closeKeysDrawer);
  if (keysBackdropEl) keysBackdropEl.addEventListener("click", closeKeysDrawer);

  if (keysFormEl) {
    keysFormEl.addEventListener("submit", (e) => {
      e.preventDefault();
      const name = keysNameEl ? keysNameEl.value.trim() : "";
      const value = keysValueEl ? keysValueEl.value : "";
      if (!name || !value) return;
      saveKey(name, value);
    });
  }
}

// --- Custody Request Cards ---

function renderCustosCard(card) {
  if (!custosCardsEl) return;
  if (custosCardsEl.querySelector('[data-card-id="' + card.id + '"]')) return;

  const cardEl = createElement("div", "custos-card custo");
  cardEl.dataset.cardId = card.id;

  const ic = createElement("div", "ic", "🛡");
  cardEl.appendChild(ic);

  const body = createElement("div", "body");
  const title = createElement("div", "custos-card-title");
  title.textContent = "Custody Request";
  const metaParts = [];
  if (card.cell) metaParts.push(card.cell);
  if (card.cred) metaParts.push(card.cred);
  if (metaParts.length > 0) {
    const metaSpan = createElement("span", "custos-card-meta custo-meta", " — " + metaParts.join(" · "));
    title.appendChild(metaSpan);
  }
  body.appendChild(title);

  if (card.dest) {
    const destEl = createElement("div", "custos-card-dest", "dest: " + card.dest);
    body.appendChild(destEl);
  }
  if (card.tool) {
    const toolEl = createElement("div", "custos-card-tool", "tool: " + card.tool);
    body.appendChild(toolEl);
  }
  if (card.review) {
    const reviewEl = createElement("div", "custos-card-review", card.review);
    body.appendChild(reviewEl);
  }
  cardEl.appendChild(body);

  const errorEl = createElement("div", "custos-card-error");
  errorEl.hidden = true;
  cardEl.appendChild(errorEl);

  const actions = createElement("div", "custos-card-actions");
  const onceBtn = createElement("button", "custobtn custos-btn-once", "Allow once");
  onceBtn.type = "button";
  const alwaysBtn = createElement("button", "custobtn custos-btn-always", "Always");
  alwaysBtn.type = "button";
  const denyBtn = createElement("button", "custobtn custos-btn-deny", "Deny");
  denyBtn.type = "button";

  let inFlight = false;
  async function resolveCard(verdict) {
    if (inFlight) return;
    inFlight = true;
    errorEl.hidden = true;
    errorEl.textContent = "";
    onceBtn.disabled = true;
    alwaysBtn.disabled = true;
    denyBtn.disabled = true;

    try {
      const resp = await fetch("/v1/custos/cards/" + encodeURIComponent(card.id) + "/resolve", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Authorization": "Bearer " + token
        },
        body: JSON.stringify({ verdict: verdict })
      });

      if (resp.status === 200) {
        cardEl.remove();
        return;
      }

      let errText = resp.statusText;
      try {
        const errJson = await resp.json();
        if (errJson && errJson.error) {
          errText = errJson.error;
        }
      } catch (_) {}

      errorEl.textContent = errText;
      errorEl.hidden = false;
      cardEl.classList.add("custos-card-error-state");

      if (resp.status !== 409) {
        onceBtn.disabled = false;
        alwaysBtn.disabled = false;
        denyBtn.disabled = false;
      }
    } catch (e) {
      errorEl.textContent = e.message;
      errorEl.hidden = false;
      onceBtn.disabled = false;
      alwaysBtn.disabled = false;
      denyBtn.disabled = false;
    } finally {
      inFlight = false;
    }
  }

  onceBtn.addEventListener("click", () => resolveCard("once"));
  alwaysBtn.addEventListener("click", () => resolveCard("always"));
  denyBtn.addEventListener("click", () => resolveCard("deny"));

  actions.appendChild(onceBtn);
  actions.appendChild(alwaysBtn);
  actions.appendChild(denyBtn);
  cardEl.appendChild(actions);

  custosCardsEl.appendChild(cardEl);
}

function removeCustosCard(id) {
  if (!custosCardsEl) return;
  const existing = custosCardsEl.querySelector('[data-card-id="' + id + '"]');
  if (existing) {
    existing.remove();
  }
}

async function loadCustosCards() {
  if (!token) return;
  try {
    const data = await fetchJson("GET", "/v1/custos/cards");
    if (data && Array.isArray(data.cards)) {
      for (const c of data.cards) {
        renderCustosCard(c);
      }
    }
  } catch (_) {}
}

let custosStreamActive = false;
async function startCustosEventStream() {
  if (!token || custosStreamActive) return;
  custosStreamActive = true;
  try {
    const resp = await fetch("/v1/custos/cards/events", {
      method: "GET",
      headers: {
        "Accept": "text/event-stream",
        "Authorization": "Bearer " + token
      }
    });
    if (!resp.ok) {
      custosStreamActive = false;
      setTimeout(startCustosEventStream, 3000);
      return;
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
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
          else if (line.startsWith("data:")) {
            try {
              eventData = JSON.parse(line.slice(5).trim());
            } catch (_) {}
          }
        }
        if (eventType === "custos_card" && eventData) {
          renderCustosCard(eventData);
        } else if (eventType === "custos_gone" && eventData) {
          removeCustosCard(eventData.id);
        }
      }
    }
  } catch (_) {
  } finally {
    custosStreamActive = false;
    setTimeout(() => {
      loadCustosCards();
      startCustosEventStream();
    }, 2000);
  }
}

// --- Initialization ---

function init() {
  initTheme();

  const host = location.hostname;
  const loopback = host === "localhost" || host === "127.0.0.1" ||
    host === "[::1]" || host === "::1" || host === "";
  if (topbarTransportEl) {
    if (loopback) {
      topbarTransportEl.textContent = "🔒 loopback";
    } else if (location.protocol === "https:") {
      topbarTransportEl.textContent = "🔒 https";
    } else {
      topbarTransportEl.textContent = "⚠️ unencrypted";
    }
  }

  initKeysDrawer();

  if (newSessionBtn) newSessionBtn.addEventListener("click", createSession);

  if (composerEl) {
    composerEl.addEventListener("submit", (e) => {
      e.preventDefault();
      const text = inputEl.value.trim();
      if (!text) return;
      inputEl.value = "";
      inputEl.style.removeProperty("height");
      sendMessage(text);
    });
  }

  if (inputEl) {
    inputEl.addEventListener("input", autoGrowTextarea);
  }

  document.addEventListener("keydown", (e) => {
    if (e.target === inputEl) {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        composerEl.requestSubmit();
      }
    } else if (e.key === "Escape") {
      if (keysDrawerEl && !keysDrawerEl.hidden) {
        closeKeysDrawer();
      } else if (document.body.classList.contains("rail-open")) {
        closeRail();
      } else if (themePopoverEl && !themePopoverEl.hidden) {
        closeThemePopover();
      }
    }
  });

  if (sessionSearchEl) {
    sessionSearchEl.addEventListener("input", () => {
      renderSessionList(loadedSessions);
    });
  }

  if (navToggleBtn) navToggleBtn.addEventListener("click", toggleRail);
  if (railBackdropEl) railBackdropEl.addEventListener("click", closeRail);

  if (themeBtnEl) themeBtnEl.addEventListener("click", toggleThemePopover);

  if (themePopoverEl) {
    for (const btn of themePopoverEl.querySelectorAll(".theme-opt")) {
      btn.addEventListener("click", () => setTheme(btn.dataset.themeVal));
    }
    for (const btn of themePopoverEl.querySelectorAll(".base-btn")) {
      btn.addEventListener("click", () => setBase(btn.dataset.baseVal));
    }
    if (accentHueEl) {
      accentHueEl.addEventListener("input", (e) => {
        const val = e.target.value;
        document.documentElement.style.setProperty("--accent-h", val);
        sessionStorage.setItem("lararium.accent-hue", val);
        if (hueValEl) hueValEl.textContent = val;
      });
    }
  }

  document.addEventListener("click", (e) => {
    if (themePopoverEl && !themePopoverEl.hidden) {
      if (!themePopoverEl.contains(e.target) && e.target !== themeBtnEl && !themeBtnEl.contains(e.target)) {
        closeThemePopover();
      }
    }
  });

  if (shareBtnEl) {
    shareBtnEl.addEventListener("click", () => {
      if (navigator.share) {
        navigator.share({ title: document.title, url: window.location.href }).catch(() => {});
      } else {
        navigator.clipboard.writeText(window.location.href).then(() => {
          const orig = shareBtnEl.textContent;
          shareBtnEl.textContent = "Copied link";
          setTimeout(() => { shareBtnEl.textContent = orig; }, 1500);
        }).catch(() => {});
      }
    });
  }

  const stopBtn = document.getElementById("stop-btn");
  if (stopBtn) stopBtn.addEventListener("click", handleStop);

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      stopCatchUpPoll();
    } else {
      if (currentSessionId) loadTranscript();
      loadCustosCards();
    }
  });

  if (!initToken()) {
    renderEmptyState();
    return;
  }

  loadSessions();
  loadCustosCards();
  startCustosEventStream();
}

document.addEventListener("DOMContentLoaded", init);
})();