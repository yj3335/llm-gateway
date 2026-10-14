# syntax=docker/dockerfile:1.7
# Builds both Go services; select with --target gateway | mockllm.
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockllm ./cmd/mockllm

FROM gcr.io/distroless/static-debian12:nonroot AS gateway
COPY --from=build /out/gateway /gateway
COPY configs/gateway.yaml /etc/llm-gateway/gateway.yaml
ENV LLMGW_CONFIG=/etc/llm-gateway/gateway.yaml
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12:nonroot AS mockllm
COPY --from=build /out/mockllm /mockllm
EXPOSE 8090
USER 65532:65532
ENTRYPOINT ["/mockllm"]
