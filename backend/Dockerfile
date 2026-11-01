FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /devath .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D app
USER app
COPY --from=build /devath /devath
ENTRYPOINT ["/devath", "-web", "-no-open", "-t", "120s"]
