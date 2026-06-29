document.addEventListener("DOMContentLoaded", () => {
  setupUploadClickTriggers();
  setupUploadDropTargets();
  setupHTMXBusyState();
});

function setupUploadClickTriggers() {
  const clickTriggers = document.querySelectorAll("[data-upload-click-trigger]");

  for (const trigger of clickTriggers) {
    const inputId = trigger.getAttribute("data-upload-input");
    const input = inputId ? document.getElementById(inputId) : null;
    if (!input) continue;

    trigger.addEventListener("click", () => input.click());

    input.addEventListener("change", () => {
      if (input.files && input.files.length > 0) {
        input.form?.requestSubmit();
      }
    });
  }
}

function setupUploadDropTargets() {
  const dropTargets = document.querySelectorAll("[data-upload-drop-target]");

  for (const target of dropTargets) {
    const inputId = target.getAttribute("data-upload-input");
    const input = inputId ? document.getElementById(inputId) : null;
    const form = input?.form ?? null;
    const queueElement = document.querySelector("[data-upload-queue]");
    if (!input || !form || !queueElement) continue;

    const queue = createUploadQueue({
      queueElement,
      target,
      form,
    });

    target.addEventListener("dragover", (event) => {
      event.preventDefault();
      target.setAttribute("data-drag-active", "true");
    });

    target.addEventListener("dragleave", (event) => {
      if (!target.contains(event.relatedTarget)) {
        target.removeAttribute("data-drag-active");
      }
    });

    target.addEventListener("drop", (event) => {
      event.preventDefault();
      target.removeAttribute("data-drag-active");
      if (!event.dataTransfer || !event.dataTransfer.files.length) return;
      queue.enqueue(event.dataTransfer.files);
    });
  }
}

function setupHTMXBusyState() {
  document.body.addEventListener("htmx:beforeRequest", (event) => {
    const target = event.target.closest("[data-busy-target]");
    if (target) target.setAttribute("data-busy", "true");
  });

  document.body.addEventListener("htmx:afterRequest", (event) => {
    const target = event.target.closest("[data-busy-target]");
    if (target) target.removeAttribute("data-busy");
  });
}

function createUploadQueue({ queueElement, target, form }) {
  const action = form.getAttribute("action") || window.location.pathname;
  const prefix = form.querySelector('input[name="prefix"]')?.value || "";
  const state = {
    entries: [],
    active: false,
  };

  renderQueue();

  return {
    enqueue(fileList) {
      for (const file of Array.from(fileList)) {
        state.entries.push({
          id: createUploadId(),
          file,
          name: file.name,
          progress: 0,
          status: "waiting",
          error: "",
        });
      }
      renderQueue();
      processQueue();
    },
  };

  async function processQueue() {
    if (state.active) return;

    const next = state.entries.find((entry) => entry.status === "waiting");
    if (!next) {
      if (!state.entries.some((entry) => entry.status === "uploading")) {
        target.removeAttribute("data-busy");
      }
      return;
    }

    state.active = true;
    target.setAttribute("data-busy", "true");
    next.status = "uploading";
    next.progress = 0;
    renderQueue();

    try {
      await uploadFile(action, prefix, next.file, (progress) => {
        next.progress = progress;
        renderQueue();
      });
      next.status = "done";
      next.progress = 100;
      renderQueue();
      await refreshFileList();
    } catch (error) {
      next.status = "failed";
      next.error = error instanceof Error ? error.message : "Upload failed";
      renderQueue();
    } finally {
      state.active = false;
      processQueue();
    }
  }

  function renderQueue() {
    const visibleEntries = state.entries.filter((entry) => entry.status !== "done");
    const completedCount = state.entries.filter((entry) => entry.status === "done").length;

    if (!visibleEntries.length && !state.active) {
      queueElement.hidden = true;
      queueElement.innerHTML = "";
      if (completedCount > 0) {
        state.entries = [];
      }
      return;
    }

    queueElement.hidden = false;
    const activeCount = state.entries.filter((entry) => entry.status === "uploading").length;
    const waitingCount = state.entries.filter((entry) => entry.status === "waiting").length;

    queueElement.innerHTML = `
      <div class="upload-queue-header">
        <div class="upload-queue-title">Uploading files</div>
        <div class="upload-queue-summary">${buildQueueSummary(activeCount, waitingCount)}</div>
      </div>
      <div class="upload-queue-list">
        ${visibleEntries.map((entry) => renderQueueItem(entry)).join("")}
      </div>
    `;
  }

  async function refreshFileList() {
    const response = await fetch(window.location.pathname + window.location.search, {
      headers: {
        "HX-Request": "true",
      },
      credentials: "same-origin",
    });
    if (!response.ok) {
      throw new Error("Could not refresh file list");
    }
    const html = await response.text();
    target.innerHTML = html;
  }
}

function buildQueueSummary(activeCount, waitingCount) {
  if (activeCount > 0 && waitingCount > 0) {
    return `${activeCount} uploading, ${waitingCount} waiting`;
  }
  if (activeCount > 0) {
    return "1 uploading";
  }
  if (waitingCount > 0) {
    return `${waitingCount} waiting`;
  }
  return "Finishing up";
}

function renderQueueItem(entry) {
  const statusText = entry.error || statusLabel(entry.status, entry.progress);
  return `
    <article class="upload-item" data-state="${entry.status}">
      <div class="upload-item-header">
        <div class="upload-item-name">${escapeHTML(entry.name)}</div>
        <div class="upload-item-meta">
          <span class="upload-item-status" data-state="${entry.status}">${escapeHTML(statusText)}</span>
          <span>${Math.round(entry.progress)}%</span>
        </div>
      </div>
      <div class="upload-item-progress" aria-hidden="true">
        <span style="width: ${entry.progress}%"></span>
      </div>
    </article>
  `;
}

function statusLabel(status, progress) {
  switch (status) {
    case "waiting":
      return "Waiting";
    case "uploading":
      return progress >= 100 ? "Processing" : "Uploading";
    case "done":
      return "Done";
    case "failed":
      return "Failed";
    default:
      return status;
  }
}

function uploadFile(action, prefix, file, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const formData = new FormData();
    formData.append("prefix", prefix);
    formData.append("file", file);

    xhr.open("POST", action, true);

    xhr.upload.addEventListener("progress", (event) => {
      if (!event.lengthComputable) return;
      onProgress((event.loaded / event.total) * 100);
    });

    xhr.addEventListener("load", () => {
      if (xhr.status >= 200 && xhr.status < 400) {
        onProgress(100);
        resolve();
        return;
      }
      reject(new Error(extractErrorMessage(xhr.responseText) || "Upload failed"));
    });

    xhr.addEventListener("error", () => {
      reject(new Error("Network error during upload"));
    });

    xhr.addEventListener("abort", () => {
      reject(new Error("Upload was cancelled"));
    });

    xhr.send(formData);
  });
}

function extractErrorMessage(responseText) {
  const trimmed = typeof responseText === "string" ? responseText.trim() : "";
  return trimmed ? trimmed : "";
}

function createUploadId() {
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function escapeHTML(value) {
  return String(value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
