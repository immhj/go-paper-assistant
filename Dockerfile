FROM golang:1.26-bookworm AS build
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/paper-assistant ./cmd/server

FROM alpine:3.22.1
RUN apk add --no-cache ca-certificates tzdata poppler-utils \
    && addgroup -S app \
    && adduser -S -G app app
WORKDIR /app
COPY --from=build /out/paper-assistant /app/paper-assistant
COPY web /app/web
RUN mkdir -p /app/data/fulltext && chown -R app:app /app
USER app
EXPOSE 8080
VOLUME ["/app/data/fulltext"]
ENTRYPOINT ["/app/paper-assistant"]
