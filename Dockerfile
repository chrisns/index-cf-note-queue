FROM --platform=$BUILDPLATFORM golang:1.27@sha256:fe517acaa580cc7915457080f850c37e618b3c1e750f98b58fc6429b605147a1 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/index-note .

FROM scratch
COPY --from=build /out/index-note /index-note
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/index-note"]
