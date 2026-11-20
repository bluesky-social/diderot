// While a job waits or runs, reload Jobs and maintenance every few seconds
// so its state and progress stay current. Only while the page is visible,
// and not once someone has typed into a form on it, which a reload would
// empty. The reload drops the notice of the action that led here, which has
// been read by then.
if (document.querySelector("[data-jobs-active]")) {
  let typed = false;
  document.addEventListener("input", () => { typed = true; });
  window.setInterval(() => {
    if (document.visibilityState !== "visible" || typed) return;
    const url = new URL(window.location.href);
    url.searchParams.delete("notice");
    url.searchParams.delete("error");
    if (url.href === window.location.href) window.location.reload();
    else window.location.replace(url);
  }, 5000);
}
