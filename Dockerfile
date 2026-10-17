# syntax=docker/dockerfile:1

ARG GO_VERSION=1.23

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/asterisk-exporter ./cmd/asterisk-exporter \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/amisim ./cmd/amisim

# Simulated Asterisk Manager Interface for the demo (docker build --target amisim).
FROM scratch AS amisim
COPY --from=build /out/amisim /amisim
USER 65532:65532
EXPOSE 5038
ENTRYPOINT ["/amisim"]
CMD ["-listen", ":5038"]

# The exporter itself: the last stage, so it is what `docker build .` produces.
FROM scratch AS exporter
COPY --from=build /out/asterisk-exporter /asterisk-exporter
LABEL org.opencontainers.image.title="asterisk-exporter" \
      org.opencontainers.image.description="Prometheus exporter for Asterisk, reading the Manager Interface (AMI)" \
      org.opencontainers.image.source="https://github.com/ErenOzgu/asterisk-exporter" \
      org.opencontainers.image.licenses="MIT"
USER 65532:65532
EXPOSE 9650
ENTRYPOINT ["/asterisk-exporter"]
