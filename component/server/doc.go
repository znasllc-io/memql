//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.1 -generate std-http,strict-server,embedded-spec -include-tags healthz,mcp,memql -package server -o server.gen.go ../schemas/open_api.yaml
//go:generate go run ../scripts/postprocess_server_gen/main.go
package server
