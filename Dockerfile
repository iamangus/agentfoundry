FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /agentfile ./cmd/agentfile/

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /agentfile /app/agentfile
COPY definitions/ /app/definitions/

EXPOSE 3000

ENTRYPOINT ["/app/agentfile"]
