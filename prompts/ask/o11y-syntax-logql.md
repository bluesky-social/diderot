## LogQL

Start with indexed stream labels, then add line filters and parsers. Structured metadata is not an indexed selector. Keep the selector and the window narrow: a broad selector over a long window is rejected or times out. The Loki stats tool is a cheap check that a stream exists and how large it is; to count matching lines, use `count_over_time`.
