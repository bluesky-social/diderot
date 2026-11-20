// While this channel's image descriptions are being rebuilt, reload the
// page every few seconds so its progress stays current.
(() => {
  if (!document.querySelector("[data-reload-while-running]")) return;
  window.setTimeout(() => window.location.reload(), 5000);
})();
