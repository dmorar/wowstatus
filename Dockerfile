FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /realm-watcher .

FROM gcr.io/distroless/static-debian12
COPY --from=build /realm-watcher /realm-watcher

ENTRYPOINT ["/realm-watcher"]
