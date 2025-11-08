FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/betsandpedestres ./cmd/betsandpedestres

FROM alpine

RUN apk add bash && addgroup -S betsandpedestres && adduser -S betsandpedestres -G betsandpedestres

USER betsandpedestres:betsandpedestres

WORKDIR /app
COPY --from=build /out/betsandpedestres ./betsandpedestres

ENV SENTRY_ENV=production

ENTRYPOINT ["/app/betsandpedestres"]
