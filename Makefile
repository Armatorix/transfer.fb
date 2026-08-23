.PHONY: lint build

lint:
	golangci-lint run --out-format=github-actions --config .golangci.yml 

build:
	docker build --build-arg RUNAS=transferfb -t transferfb:latest .