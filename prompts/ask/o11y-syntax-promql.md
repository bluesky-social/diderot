## PromQL

Look up metric and label names before using them. For a counter, use `rate` or `increase` over a range, and keep the labels the question needs when you aggregate. For a histogram, apply `histogram_quantile` to the `rate` of the `_bucket` series, grouped by `le` and the dimensions the question needs. Do not average quantiles.
