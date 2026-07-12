FROM golang:1.22 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /server .

FROM alpine:3.20
RUN apk add --no-cache wget && \
    addgroup -S app && adduser -S app -G app
COPY --from=builder /server /server
EXPOSE 8080
USER app
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1
ENTRYPOINT ["/server"]