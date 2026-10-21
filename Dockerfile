FROM --platform=$BUILDPLATFORM golang:1.27@sha256:f24164268fc1e741bbdc0066dbadcf1c8426c5a8489f0729c02c3905a53032fd AS build
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
