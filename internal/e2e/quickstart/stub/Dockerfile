# The quickstart smoke's stand-in model provider and embedder. Built from the
# repository root (compose.smoke.yaml); the stub uses only the standard library.
FROM docker.io/library/golang:1.27.4-bookworm@sha256:ce59b41415e2524ae99926fe1f37ce48ba8c194dcbd2a56a65436cdcefa2bd34 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/e2e/quickstart/stub ./internal/e2e/quickstart/stub
RUN CGO_ENABLED=0 go build -mod=mod -buildvcs=false -o /out/stub ./internal/e2e/quickstart/stub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/stub /usr/local/bin/stub
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/stub"]
