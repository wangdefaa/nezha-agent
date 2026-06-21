# 哪吒监控 Agent 镜像(多阶段构建,纯 Go、CGO 关闭)
FROM golang:alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /nezha-agent ./cmd/agent

FROM alpine
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /nezha-agent /usr/bin/nezha-agent
ENTRYPOINT ["/usr/bin/nezha-agent"]
