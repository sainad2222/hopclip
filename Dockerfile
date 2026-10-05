# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/hopclip ./cmd/hopclip \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="Hopclip" \
      org.opencontainers.image.description="Self-hosted clipboard and file sharing across your devices" \
      org.opencontainers.image.source="https://github.com/sainad2222/hopclip" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/hopclip /usr/local/bin/hopclip
# A fresh named volume inherits this ownership, so the non-root user can write.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data LISTEN_ADDR=:8080
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["hopclip", "healthcheck"]
ENTRYPOINT ["hopclip"]
CMD ["serve"]
