# The quickstart smoke's stand-in model provider and embedder. Built from the
# repository root (compose.smoke.yaml); the stub uses only the standard library.
FROM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/e2e/quickstart/stub ./internal/e2e/quickstart/stub
RUN CGO_ENABLED=0 go build -mod=mod -buildvcs=false -o /out/stub ./internal/e2e/quickstart/stub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/stub /usr/local/bin/stub
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/stub"]
