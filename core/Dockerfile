# swarmpool-core 多阶段构建（在 vps87 上本地构建，避开窄带宽拉大镜像）
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
# go.sum 由 go mod tidy 生成后一起 COPY；构建时自动补齐依赖
RUN go mod download || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/swarmpool-core .

FROM alpine:3.20
RUN adduser -D -u 10001 core
USER core
COPY --from=build /out/swarmpool-core /usr/local/bin/swarmpool-core
EXPOSE 8080
ENTRYPOINT ["swarmpool-core"]
