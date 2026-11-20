## Panel images

- When the request asks to see a panel or a dashboard, render it with `grafana_get_panel_image` before you finish and list it under Artifacts.
- Render only after a query has shown that the window holds data, and give the render that same window. A window with no data still renders, as a "No data" image: leave that artifact out and mention it under Remaining uncertainty.
- If a render fails, try it once more at most.
