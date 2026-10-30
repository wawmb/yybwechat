# =============================================================================
# yybwechat — 微信扫码登录服务 Docker 镜像（仅 AMD64）
# =============================================================================
#
# 目录分离：模板/静态资源 (/app/resource) 随镜像更新，
# 运行数据 (/app/data) 可挂载卷持久化，互不干扰。
#
# === 构建 ===
#   docker buildx build --platform linux/amd64 -t wawmb/yybwechat:latest --no-cache --progress=plain .
#
# === 运行 ===
#   # 最简（数据随容器）
#   docker run -d --name yybwechat -p 60006:8000 wawmb/yybwechat:latest
#
#   # 持久化
#   docker run -d --name yybwechat -p 60006:8000 \
#     -v /host/data:/app/data wawmb/yybwechat:latest
#
# === 更新 ===
#   docker rm -f yybwechat
#   docker buildx build --platform linux/amd64 -t wawmb/yybwechat:latest --no-cache --progress=plain .
#   docker run -d --name yybwechat --restart=always -p 60006:8000 -v yybwechat-data:/app/data wawmb/yybwechat:latest
# =============================================================================

# ---------- 第一阶段: 编译 ----------
# 使用 $BUILDPLATFORM 让编译阶段始终在原生平台运行（更快），
# Go 通过 GOOS/GOARCH 交叉编译为目标平台 amd64。
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

# 设置 Go 模块代理为国内镜像，加速依赖下载
ENV GOPROXY=https://goproxy.cn,direct
ENV GONOSUMDB=*

WORKDIR /src

# 先复制依赖文件以利用 Docker 缓存层
COPY go.mod go.sum ./
RUN go mod download

# 复制全部源码
COPY . .

# 编译为静态链接的二进制文件
# modernc.org/sqlite 是纯 Go 驱动，无需 CGO 即可静态编译
# -s -w — 去除调试信息与符号表，减小体积
# -trimpath — 从二进制中移除文件系统路径信息
RUN CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/yybwechat \
    ./cmd/yybwechat/

# ---------- 第二阶段: 运行 ----------
# 不指定 --platform，继承 docker buildx --platform 参数
FROM alpine:3.21

# 安装运行时依赖
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    curl \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

# 镜像元数据
LABEL org.opencontainers.image.title="yybwechat" \
      org.opencontainers.image.description="微信扫码登录服务" \
      org.opencontainers.image.architecture="amd64" \
      org.opencontainers.image.vendor="yybwechat" \
      org.opencontainers.image.source="https://github.com/yybwechat/yybwechat"

# 创建非 root 用户
RUN addgroup -g 1000 appgroup && \
    adduser -D -u 1000 -G appgroup appuser

WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /out/yybwechat .

# 创建目录结构：模板/静态资源与数据分离
# /app/resource — 模板和静态资源（随镜像更新）
# /app/resource/templates — HTML 模板
# /app/resource/static — 静态资源（CSS/JS/图片）
# /app/data — 运行时数据（可挂载卷持久化）
# /app/data/db — SQLite 数据库
# /app/data/avatars — 头像缓存
# /app/data/qr — 二维码缓存
COPY resource/templates/ /app/resource/templates/
COPY resource/static/   /app/resource/static/
RUN mkdir -p /app/data/db /app/data/avatars /app/data/qr \
    && chown -R appuser:appgroup /app /app/data /app/resource

# 不声明 VOLUME — 避免匿名卷在镜像更新后缓存旧模板/静态文件。
# 需要持久化数据时，手动挂载: -v /host/data:/app/data

# 切换到非 root 用户
USER appuser

# 暴露默认端口
EXPOSE 8000

# 健康检查
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -sf http://127.0.0.1:8000/health || exit 1

# 默认启动参数
ENTRYPOINT ["./yybwechat"]
CMD ["--host", "0.0.0.0", "--port", "8000", "--resource-root", "/app/resource", "--data-root", "/app/data"]
