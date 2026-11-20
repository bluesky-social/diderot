## TraceQL

Write attributes with their scope (`resource.` or `span.`), and look up names and values when unsure. Search for a narrow set of traces first. Fetch a whole trace by ID only when a claim about its spans needs it, since that is slow and can time out. For totals and rates use TraceQL metrics, not a handful of fetched traces.
