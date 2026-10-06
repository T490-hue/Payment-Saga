FROM golang:1.22-alpine AS build
ARG SERVICE
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/${SERVICE} ./cmd/${SERVICE}

FROM alpine:3.20
ARG SERVICE
RUN apk add --no-cache ca-certificates wget && adduser -D -u 65532 app
COPY --from=build /out/${SERVICE} /usr/local/bin/service
USER app
EXPOSE 8080
ENTRYPOINT ["service"]
