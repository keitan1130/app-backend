FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12

ENV PORT=8080
EXPOSE 8080

COPY --from=builder /out/server /server

USER nonroot:nonroot
ENTRYPOINT ["/server"]
