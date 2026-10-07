.PHONY: test vet lint build engine-example clean

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

build:
	go build -o provenance ./cmd/provenance/

# Compile the standalone external-consumer module that imports the public
# engine facade.
engine-example:
	cd engine/externaltest && go build ./...

clean:
	rm -f provenance

all: vet lint test build engine-example
