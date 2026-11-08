.DEFAULT_GOAL := help

GO ?= go
BINARY ?= bastionctl
ARGS ?=

.PHONY: help build run test

help:
	@printf '%s\n' \
		'make build                 Compile the application' \
		'make run [ARGS="..."]     Build and run the application' \
		'make test                  Run all Go tests'

build:
	$(GO) build -o $(BINARY) .

run: build
	./$(BINARY) $(ARGS)

test:
	$(GO) test ./...