# 0.0 shouldn't clobber any release builds
PREFIX="zlabjp/nghttpx-ingress-controller"
TAG=latest

REPO_INFO=$(shell git config --get remote.origin.url)

ifndef VERSION
  VERSION := git-$(shell git rev-parse --short HEAD)
endif

export GO111MODULE=on

.PHONY: all
all: push

.PHONY: controller
controller:
	CGO_ENABLED=0 GOOS=linux go build -installsuffix cgo -ldflags \
		"-w -X main.version=${VERSION} -X main.gitRepo=${REPO_INFO}" \
		github.com/zlabjp/nghttpx-ingress-lb/cmd/nghttpx-ingress-controller/...
	CGO_ENABLED=0 GOOS=linux go build -installsuffix cgo \
		github.com/zlabjp/nghttpx-ingress-lb/cmd/fetch-ocsp-response/...
	CGO_ENABLED=0 GOOS=linux go build -installsuffix cgo \
		github.com/zlabjp/nghttpx-ingress-lb/cmd/cat-ocsp-resp/...

.PHONY: container
container: controller
	docker build -t "${PREFIX}:${TAG}" .

.PHONY: push
push: container
	docker push "${PREFIX}:${TAG}"

.PHONY: clean
clean:
	rm -f nghttpx-ingress-controller

.PHONY: vet
vet:
	go vet -printfuncs Infof,Warningf,Errorf,Fatalf,Exitf github.com/zlabjp/nghttpx-ingress-lb/pkg/... github.com/zlabjp/nghttpx-ingress-lb/cmd/...

.PHONY: fmt
fmt:
	go fmt github.com/zlabjp/nghttpx-ingress-lb/pkg/... github.com/zlabjp/nghttpx-ingress-lb/cmd/...

.PHONY: check
check:
	go test github.com/zlabjp/nghttpx-ingress-lb/pkg/... github.com/zlabjp/nghttpx-ingress-lb/cmd/...
