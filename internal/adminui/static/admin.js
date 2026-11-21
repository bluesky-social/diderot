// Progressive enhancement only. Every administrator workflow must remain
// usable through ordinary links and forms when JavaScript is unavailable.
document.documentElement.classList.add("js");

async function copyText(text) {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall through for non-secure contexts and denied clipboard access.
    }
  }
  const field = document.createElement("textarea");
  field.value = text;
  field.setAttribute("readonly", "");
  field.style.position = "fixed";
  field.style.opacity = "0";
  document.body.appendChild(field);
  field.select();
  const copied = document.execCommand("copy");
  field.remove();
  if (!copied) throw new Error("copy command failed");
}

// A copy button: <button data-copy-text="…">, then optionally an element
// with role="status" that announces the result.
for (const button of document.querySelectorAll("[data-copy-text]")) {
  const label = button.textContent;
  const status = button.nextElementSibling?.matches("[role=status]") ? button.nextElementSibling : null;
  button.addEventListener("click", async () => {
    try {
      await copyText(button.dataset.copyText);
      button.textContent = "Copied";
      if (status) status.textContent = "Copied";
      window.setTimeout(() => {
        button.textContent = label;
        if (status) status.textContent = "";
      }, 1600);
    } catch {
      if (status) status.textContent = "Copy failed";
    }
  });
}

// Times arrive as <time datetime title data-time> with UTC text (format.go
// timeTag). Show the exact instant in the viewer's zone in the title, keeping
// the UTC value beside it, and redraw a date, and a date with its time of
// day, in the viewer's zone.
const exactTimeFormatter = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "long",
});
const thisYear = new Date().getFullYear();
for (const element of document.querySelectorAll("time[data-time][datetime]")) {
  const value = new Date(element.dateTime);
  if (Number.isNaN(value.getTime())) continue;
  const utc = element.title;
  element.title = utc ? `${exactTimeFormatter.format(value)} (${utc})` : exactTimeFormatter.format(value);
  if (element.dataset.time === "date" || element.dataset.time === "datetime") {
    const options = { month: "short", day: "numeric" };
    if (value.getFullYear() !== thisYear) options.year = "numeric";
    if (element.dataset.time === "datetime") Object.assign(options, { hour: "numeric", minute: "2-digit" });
    element.textContent = new Intl.DateTimeFormat(undefined, options).format(value);
  }
}

// The result of an action arrives as ?notice= or ?error= after a redirect.
// Drop it from the address once shown, so a reload does not repeat it.
const address = new URL(window.location.href);
if (address.searchParams.has("notice") || address.searchParams.has("error")) {
  address.searchParams.delete("notice");
  address.searchParams.delete("error");
  window.history.replaceState(null, "", address);
}

// Browsers do not put generic overflow containers in the keyboard tab order.
// Add focus only when content actually overflows, so short ledgers and prompt
// bodies do not create dead tab stops. Recheck when a details element opens or
// the viewport changes.
const scrollRegions = document.querySelectorAll([
  "pre",
  ".table-wrap",
  ".tabs",
  ".prose table",
  ".app-header nav",
].join(","));

function refreshScrollRegionFocus() {
  for (const region of scrollRegions) {
    const overflows = region.scrollWidth > region.clientWidth || region.scrollHeight > region.clientHeight;
    if (overflows) {
      region.tabIndex = 0;
    } else {
      region.removeAttribute("tabindex");
    }
  }
}

refreshScrollRegionFocus();
window.addEventListener("resize", refreshScrollRegionFocus, { passive: true });
document.addEventListener("toggle", refreshScrollRegionFocus, true);

// Use the selected Ask model's published or tested efforts when available.
// The form still submits and validates normally without JavaScript.
const askModel = document.getElementById("role-ask");
const askEffort = document.getElementById("ask-reasoning-effort");
if (askModel && askEffort) {
  const updateEfforts = () => {
    const model = askModel.selectedOptions[0];
    const known = (model?.dataset.reasoningEfforts || "").trim().split(/\s+/).filter(Boolean);
    const defaultEffort = model?.dataset.defaultEffort;
    askEffort.options[0].textContent = defaultEffort ? `Model default (${defaultEffort})` : "Model default";
    for (const option of askEffort.options) {
      option.disabled = known.length > 0 && option.value !== "" && !known.includes(option.value) && !option.selected;
    }
    const unsupported = known.length > 0 && askEffort.value !== "" && !known.includes(askEffort.value);
    askEffort.setCustomValidity(unsupported ? "Choose a reasoning effort supported by this model." : "");
  };
  askModel.addEventListener("change", updateEfforts);
  askEffort.addEventListener("change", updateEfforts);
  updateEfforts();
}
