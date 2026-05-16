# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG BUILT_BY=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags="-s -w \
  -X github.com/nicolerenee/promptbook/internal/version.Version=${VERSION} \
  -X github.com/nicolerenee/promptbook/internal/version.Commit=${COMMIT} \
  -X github.com/nicolerenee/promptbook/internal/version.BuildDate=${BUILD_DATE} \
  -X github.com/nicolerenee/promptbook/internal/version.BuiltBy=${BUILT_BY}" \
  -o /promptbook .

# Runtime stage
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source="https://github.com/nicolerenee/promptbook"
LABEL org.opencontainers.image.description="Encora-backed catalog and library tools for Broadway recordings"
LABEL org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /promptbook /usr/local/bin/promptbook

EXPOSE 8080

ENTRYPOINT ["promptbook"]
