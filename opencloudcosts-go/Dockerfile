FROM golang:1.25.12-alpine AS builder
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-X main.version=${VERSION}" \
      -o /opencloudcosts ./cmd/opencloudcosts

FROM scratch
COPY --from=builder /opencloudcosts /opencloudcosts
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8080
ENTRYPOINT ["/opencloudcosts"]
CMD ["--transport", "http", "--host", "0.0.0.0", "--port", "8080"]
