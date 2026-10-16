FROM golang:1.27.2-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Local builds use the embedded development version; CI supplies source identity.
ARG VERSION
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/sortedcord/codex-proxy/internal/buildinfo.Version=${VERSION} -X github.com/sortedcord/codex-proxy/internal/buildinfo.Commit=${COMMIT} -X github.com/sortedcord/codex-proxy/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/codex-proxy ./cmd/codex-proxy \
    && mkdir -p /home/app/.codex /home/app/.local/share/codex-proxy \
    && chmod 0700 /home/app/.codex /home/app/.local/share/codex-proxy

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/codex-proxy /usr/local/bin/codex-proxy
COPY --from=build --chown=10001:10001 /home/app/ /home/app/

ENV HOME=/home/app \
    CODEX_HOME=/home/app/.codex \
    CODEX_PROXY_ADDR=0.0.0.0:8080 \
    CODEX_PROXY_HISTORY_DIR=/home/app/.local/share/codex-proxy

USER 10001:10001
WORKDIR /home/app
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/codex-proxy"]
CMD ["serve"]
