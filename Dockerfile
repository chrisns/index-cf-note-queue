FROM --platform=$BUILDPLATFORM golang:1.27@sha256:007a316059246276cb214df93950be77837545dc78007a73e0291a4963d5d3bf AS build
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
