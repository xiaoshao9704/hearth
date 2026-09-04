# Hearth 一体化镜像：前端构建 + Go 后端 + 精简运行时
# 构建上下文为仓库根：docker build -t hearth .

# ---- 前端构建 ----
FROM node:22-alpine AS web
WORKDIR /build
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# 生产同源部署：置空使 API 走相对路径（LiveKit 地址由 server 下发，兜底置空）
ENV VITE_SERVER_URL="" VITE_LIVEKIT_URL=""
RUN npm run build

# ---- Go 后端构建（纯 Go sqlite，无 cgo；前端产物拷入 embed 目录编进二进制）----
FROM golang:1.27-alpine AS server
WORKDIR /build
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY --from=web /build/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go build -o /hearth ./cmd/server \
  && mkdir -p /data # distroless 无 shell，/data 在构建阶段备好

# ---- 运行时（distroless/static：仅二进制 + CA 证书 + nonroot 用户，无 shell）----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /hearth /app/hearth
COPY --from=server --chown=65532:65532 /data /data
ENV ADDR=:8080 \
    DB_PATH=/data/hearth.db
VOLUME /data
USER 65532
EXPOSE 8080
ENTRYPOINT ["/app/hearth"]
