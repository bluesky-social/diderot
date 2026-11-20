// Progressive enhancement for the report pages. Without it, an example link
// reloads the new-report page with the example filled in, and the runs list
// has a Refresh link.

// An example fills in the instructions where they are, keeping what else
// was typed, and the name when it is empty.
for (const link of document.querySelectorAll("a[data-example]")) {
  link.addEventListener("click", (event) => {
    const example = document.getElementById(link.dataset.example);
    const prompt = document.getElementById("report-prompt");
    if (!example || !prompt) return;
    event.preventDefault();
    const text = example.content.textContent.trim();
    if (prompt.value.trim() && prompt.value.trim() !== text &&
        !window.confirm("Replace the instructions with this example?")) {
      return;
    }
    prompt.value = text;
    const name = document.getElementById("report-name");
    if (name && !name.value.trim()) name.value = link.dataset.name || "";
    prompt.focus();
  });
}

// While a run waits or is in progress, reload to show how it went, and say
// so. The reload drops the query, so the result of the action that led here
// ("Run queued.") is not repeated once it is out of date.
const refresh = document.querySelector("[data-refresh]");
if (refresh) {
  const seconds = Number(refresh.dataset.refresh);
  if (seconds > 0) {
    const note = refresh.querySelector("[data-refresh-note]");
    if (note) note.textContent = `A run is in progress. This page updates every ${seconds} seconds.`;
    window.setTimeout(() => window.location.replace(window.location.pathname), seconds * 1000);
  }
}
