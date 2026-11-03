FROM golang:1.26.1 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /opencanon ./cmd/opencanon

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils cups-client cups-ipp-utils imagemagick img2pdf \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home opencanon \
    && mkdir -p /var/lib/opencanon && chown opencanon:opencanon /var/lib/opencanon
WORKDIR /app
COPY --from=build /opencanon /usr/local/bin/opencanon
COPY config/printer.json /app/config/printer.json
USER opencanon
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/opencanon"]
CMD ["-listen", "0.0.0.0:8080", "-printer-config", "/app/config/printer.json", "-data-dir", "/var/lib/opencanon", "-pdfinfo", "/usr/bin/pdfinfo"]
