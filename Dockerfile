FROM golang:1.26.1 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /opencanon ./cmd/opencanon

FROM debian:bookworm-slim AS renderer
RUN apt-get update && apt-get install -y --no-install-recommends build-essential pkg-config curl ca-certificates xz-utils patch flex gettext \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/native/render
COPY native/render ./
RUN make

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils imagemagick img2pdf \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home opencanon \
    && mkdir -p /var/lib/opencanon && chown opencanon:opencanon /var/lib/opencanon
WORKDIR /app
COPY --from=build /opencanon /usr/local/bin/opencanon
COPY --from=renderer /src/native/render/opencanon-render /usr/local/bin/opencanon-render
COPY --from=renderer /opt/opencanon/gutenprint /opt/opencanon/gutenprint
COPY native/render /usr/share/opencanon/native-render
COPY config/printer.json /app/config/printer.json
USER opencanon
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/opencanon"]
CMD ["-listen", "0.0.0.0:8080", "-printer-config", "/app/config/printer.json", "-data-dir", "/var/lib/opencanon", "-pdfinfo", "/usr/bin/pdfinfo"]
