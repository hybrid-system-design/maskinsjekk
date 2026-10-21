# syntax=docker/dockerfile:1

# --- build stage -------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bambu-bridge .

# --- runtime stage (static, minimal) ----------------------------------------
FROM scratch
COPY --from=build /bambu-bridge /bambu-bridge
ENTRYPOINT ["/bambu-bridge"]
