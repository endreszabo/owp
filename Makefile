all:
	protoc --go_out=. payload.proto
	go build -buildmode=pie -trimpath -ldflags=-extldflags=-static\ -linkmode=static -tags netgo -mod=readonly -modcacherw -ldflags="-s -w"
	#-X main.GitCommit=$(shell git describe --dirty --always)"

