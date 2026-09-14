BINARY := ./bin/km

.PHONY: build test vet fmt fmt-check integration clean
build:
	go build -o $(BINARY) ./cmd/km

test:
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l . )" || { echo "未格式化文件:"; gofmt -l .; exit 1; }

# 真实集成套件：需要本机 Docker 引擎与最小镜像（缺失时 TestMain 预检显式 SKIP 或构建）
integration:
	go test -tags=integration -count=1 -timeout 15m ./tests/integration/

clean:
	rm -rf bin

all: fmt-check vet test build
