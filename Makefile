build:	test
	go build ./app
	go build -o ${GOPATH}/bin/proxy-forwarding-agent ./agent/agent.go

test:	vet
	go test ./agent/utils/...
	go test -count 1 ./agent/agent_test.go

vet:	fmt
	go vet ./agent/utils/...
	go vet ./agent/agent.go
	go vet ./app/...

fmt:	FORCE
	gofmt -w ./

deploy:
	if [ -z "${PROJECT_ID}" ]; then echo "You must specify the PROJECT_ID"; exit 1; fi
	gcloud app deploy --project "${PROJECT_ID}" --version v1 ./app/*.yaml

FORCE:
