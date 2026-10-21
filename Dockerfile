# siprec-receiver — SIPREC Session Recording Server.
# Multi-stage: CGO build (SQLite) in the Go image, slim runtime.
#   docker build -t siprec-receiver .
#   docker run --rm -p 5070:5070/udp -p 5070:5070/tcp -p 8080:8080 \
#     -v siprec-data:/data --env-file .env siprec-receiver
# SIPREC_LOCAL_IP must be an address the SRC can reach for RTP; in a
# container deployment that is normally the host's address (or use
# --network host and keep the default loopback listeners).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -o /out/siprec-receiver ./cmd/siprec-receiver

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/siprec-receiver /usr/local/bin/siprec-receiver
ENV RECORDING_DIR=/data/recordings \
    DB_PATH=/data/siprec-receiver.db \
    HTTP_LISTEN=0.0.0.0:8080 \
    SIPREC_LISTEN=0.0.0.0:5070
VOLUME /data
EXPOSE 5070/udp 5070/tcp 8080/tcp
ENTRYPOINT ["/usr/local/bin/siprec-receiver"]
