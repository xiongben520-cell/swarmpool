FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/spark .

FROM alpine:3.20
RUN adduser -D -u 10001 spark
USER spark
COPY --from=build /out/spark /usr/local/bin/spark
ENTRYPOINT ["spark"]
