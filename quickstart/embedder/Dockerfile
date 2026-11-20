# The quickstart's local embedder: Hugging Face text-embeddings-inference (TEI)
# on CPU. TEI publishes amd64 and arm64 CPU builds under separate tags rather
# than one multi-arch image, so this picks the pinned build for the host's
# architecture. The model is downloaded into the /data volume on first start.
ARG TARGETARCH

FROM ghcr.io/huggingface/text-embeddings-inference:cpu-1.9.4@sha256:2538ea1c9640d3763b15af668039d24172d063b42337b0c27796fc2be180c78d AS tei-amd64
FROM ghcr.io/huggingface/text-embeddings-inference:cpu-arm64-1.9.4@sha256:798cd7615e060a72ccbdf19aa0ada52289c6a0147490195bafaece6ce04d70f1 AS tei-arm64

FROM tei-${TARGETARCH}
