build:	test
	go build ./app

test:	buildrunlocal
	go test ./agent/sessions/...
	go test ./agent/utils/...
	go test -count 1 ./agent/agent_test.go

buildrunlocal: buildagent buildserver
	go build -o ${GOPATH}/bin/inverting-proxy-run-local ./testing/runlocal/main.go

buildagent: vet
	go build -o ${GOPATH}/bin/proxy-forwarding-agent ./agent/agent.go

buildserver: vet
	go build -o ${GOPATH}/bin/inverting-proxy ./server/server.go

vet:	deps
	go vet ./agent/sessions/...
	go vet ./agent/utils/...
	go vet ./agent/websockets/...
	go vet ./agent/agent.go
	go vet ./app/...

deps:	fmt
	go get ./...

fmt:	FORCE
	gofmt -w ./

deploy:
	if [ -z "${PROJECT_ID}" ]; then echo "You must specify the PROJECT_ID"; exit 1; fi
	gcloud app deploy --project "${PROJECT_ID}" --version v1 ./app/*.yaml

FORCE:
