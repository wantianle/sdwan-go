VERSION  ?= 1.0.1
BUILDDATE = $(shell date '+%Y-%m-%d %H:%M:%S')
LDFLAGS  := -s -w -X main.Version=$(VERSION) -X 'main.BuildDate=$(BUILDDATE)'
GOPROXY  ?= https://goproxy.cn,direct
DST       = dist
BUILD_DIR = cmd/sdwan

.PHONY: all linux macos windows panel clean test vet tidy

all: linux macos windows panel
	@ls -lh $(DST)/

linux:
	cd $(BUILD_DIR) && GOOS=linux   GOARCH=amd64 GOPROXY=$(GOPROXY) go build -ldflags "$(LDFLAGS)" -o ../../$(DST)/sdwan-linux-amd64          .

macos:
	cd $(BUILD_DIR) && GOOS=darwin  GOARCH=amd64 GOPROXY=$(GOPROXY) go build -ldflags "$(LDFLAGS)" -o ../../$(DST)/sdwan-macos-amd64           .
	cd $(BUILD_DIR) && GOOS=darwin  GOARCH=arm64 GOPROXY=$(GOPROXY) go build -ldflags "$(LDFLAGS)" -o ../../$(DST)/sdwan-macos-arm64           .

windows:
	cd $(BUILD_DIR) && GOOS=windows GOARCH=amd64 GOPROXY=$(GOPROXY) go build -ldflags "$(LDFLAGS)" -o ../../$(DST)/sdwan-windows-amd64.exe     .

panel:
	cd panel && GOPROXY=$(GOPROXY) go mod tidy && GOOS=windows GOARCH=amd64 go build -tags production -ldflags="-s -w -H windowsgui" -o ../dist/sdwan-panel.exe .

clean:
	rm -f $(DST)/sdwan-*

test:
	go test -v -race -count=1 ./...

vet:
	go vet ./...

tidy:
	go mod tidy