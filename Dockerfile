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

# ---- Go 后端构建（纯 Go sqlite，无 cgo）----
FROM golang:1.27-alpine AS server
WORKDIR /build
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -o /hearth ./cmd/server

# ---- 运行时 ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
  && adduser -D -u 10001 hearth \
  && mkdir -p /data && chown hearth:hearth /data
COPY --from=server /hearth /app/hearth
COPY --from=web /build/dist /app/web
ENV ADDR=:8080 \
    DB_PATH=/data/hearth.db \
    STATIC_DIR=/app/web
VOLUME /data
USER hearth
EXPOSE 8080
ENTRYPOINT ["/app/hearth"]
