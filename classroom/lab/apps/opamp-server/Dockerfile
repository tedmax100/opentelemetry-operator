# ---- build stage ----
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o /opamp-server .

# ---- runtime stage ----
FROM gcr.io/distroless/static:nonroot
COPY --from=build /opamp-server /opamp-server
EXPOSE 4320 8080
ENTRYPOINT ["/opamp-server"]
