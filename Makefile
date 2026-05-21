.PHONY: crawl crawl-rfcs crawl-drafts build vet test

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

crawl: build
	go run ./cmd/ietf-crawl --corpus .

crawl-rfcs: build
	go run ./cmd/ietf-crawl --corpus . --source rfcs

crawl-drafts: build
	go run ./cmd/ietf-crawl --corpus . --source drafts
