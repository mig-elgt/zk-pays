FROM golang:1.22 AS builder
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -v -a -installsuffix cgo -o /bin/app ./cmd/payments/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /bin/app /usr/local/bin/app
RUN chmod +x /usr/local/bin/app

CMD ["app"]
EXPOSE 8080
