# Copyright 2017 Google Inc. All rights reserved.
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
# http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM debian

RUN apt-get update && apt-get upgrade -y && \
    apt-get install -y -qq --no-install-recommends \
      ca-certificates \
      git \
      wget && \
    mkdir -p /opt/bin && \
    mkdir -p /opt/src/github.com/google/inverting-proxy

ADD ./ /opt/src/github.com/google/inverting-proxy

RUN wget -O /opt/go1.11.1.linux-amd64.tar.gz \
      https://storage.googleapis.com/golang/go1.11.1.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf /opt/go1.11.1.linux-amd64.tar.gz && \
    export PATH=${PATH}:/usr/local/go/bin/:/opt/bin/ && \
    export GOPATH=/opt/ && \
    go get github.com/golang/groupcache/lru && \
    go get golang.org/x/net/context && \
    go get golang.org/x/oauth2/google && \
    go get google.golang.org/api/compute/v1 && \
    go build -o ${GOPATH}/bin/proxy-forwarding-agent /opt/src/github.com/google/inverting-proxy/agent/agent.go && \
    rm -rf /opt/go1.4.2.linux-amd64.tar.gz && \
    rm -rf /usr/local

ENV DEBUG "false"
ENV PROXY ""
ENV PROXY_TIMEOUT "60s"
ENV BACKEND ""
ENV HOSTNAME "localhost"
ENV PORT "8080"
ENV SHIM_WEBSOCKETS "false"
ENV SHIM_PATH ""
ENV HEALTH_CHECK_PATH "/"
ENV HEALTH_CHECK_INTERVAL_SECONDS "0"
ENV HEALTH_CHECK_UNHEALTHY_THRESHOLD "2"

CMD ["/bin/sh", "-c", "/opt/bin/proxy-forwarding-agent --debug=${DEBUG} --proxy=${PROXY} --proxy-timeout=${PROXY_TIMEOUT} --backend=${BACKEND} --host=${HOSTNAME}:${PORT} --shim-websockets=${SHIM_WEBSOCKETS} --shim-path=${SHIM_PATH} --health-check-path=${HEALTH_CHECK_PATH} --health-check-interval-seconds=${HEALTH_CHECK_INTERVAL_SECONDS} --health-check-unhealthy-threshold=${HEALTH_CHECK_UNHEALTHY_THRESHOLD}" ]
