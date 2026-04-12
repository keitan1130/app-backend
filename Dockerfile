FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server

FROM python:3.12-slim AS runtime

ENV PORT=8080
ENV PYTHONUNBUFFERED=1
EXPOSE 8080

RUN pip install --no-cache-dir "markitdown[all]" \
	&& useradd --system --no-create-home --uid 10001 appuser

COPY --from=builder /out/server /server

USER appuser
ENTRYPOINT ["/server"]
