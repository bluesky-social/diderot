## Grafana and Tempo

{{GRAFANA_DATASOURCES}}

- Look up the real names of metrics, labels, log streams, profile types and trace attributes with the discovery tools before you use them in a query; do not guess. Then run the narrowest query that answers the question, and widen it only when the result requires.
- A datasource is addressed by its UID. For a kind not given above, find the UID with the datasource tools and never guess one. Tempo tools take no datasource UID.
