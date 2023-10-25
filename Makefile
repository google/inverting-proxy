build:	test
	go build ./app/...
	go mod tidy

test:	buildrunlocal buildrunwebsockets buildtcpbridge
	go test ./agent/banner/...
	go test ./agent/metrics/...
	go test ./agent/sessions/...
	go test ./agent/utils/...
	go test ./agent/websockets/...
	go test -count 1 ./agent/agent_test.go
	go mod tidy

buildtcpbridge: vet
	go build -o ${GOPATH}/bin/tcp-over-ws-bridge-frontend ./utils/tcpbridge/frontend/frontend.go
	go build -o ${GOPATH}/bin/tcp-over-ws-bridge-backend ./utils/tcpbridge/backend/backend.go
	go mod tidy

buildrunlocal: buildagent buildserver
	go build -o ${GOPATH}/bin/inverting-proxy-run-local ./testing/runlocal/main.go
	go mod tidy

buildrunwebsockets: buildagent buildserver
	go build -o ${GOPATH}/bin/inverting-proxy-run-websockets ./testing/websockets/main.go
	go build -o ${GOPATH}/bin/example-websocket-server ./testing/websockets/example/main.go
	go mod tidy

buildagent: vet
	go build -o ${GOPATH}/bin/proxy-forwarding-agent ./agent/agent.go
	go mod tidy

buildserver: vet
	go build -o ${GOPATH}/bin/inverting-proxy ./server/server.go
	go mod tidy

vet:	deps
	go vet ./agent/banner/...
	go vet ./agent/metrics/...
	go vet ./agent/sessions/...
	go vet ./agent/utils/...
	go vet ./agent/websockets/...
	go vet ./agent/agent.go
	go vet ./app/...
	go mod tidy

deps:	fmt
	go get ./...
	go mod tidy

fmt:	FORCE
	gofmt -w ./
	go mod tidy

deploy:
	if [ -z "${PROJECT_ID}" ]; then echo "You must specify the PROJECT_ID"; exit 1; fi
	gcloud app deploy --project "${PROJECT_ID}" --version v1 ./app/*.yaml

FORCE:
