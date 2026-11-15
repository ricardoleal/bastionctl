# Build and development

The Makefile provides common local workflows:

```bash
make help
make build
make test
make run
make run ARGS="--aws-profile prod"
```

`make run` rebuilds the binary before launching it.

Cross-compile static binaries without CGO:

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/bastionctl-darwin-arm64 .
GOOS=darwin GOARCH=amd64 go build -o bin/bastionctl-darwin-amd64 .
GOOS=linux GOARCH=amd64 go build -o bin/bastionctl-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o bin/bastionctl-linux-arm64 .
```