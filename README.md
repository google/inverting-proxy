# Inverting Proxy and Agent

This repository defines a reverse proxy that inverts the direction of traffic
between the proxy and the backend servers.

That design makes configuring and hosting backends much simpler than what
is required for traditional reverse proxies:

1. The communication channel between the proxy and backends can be secured
   using the SSL certificate of the proxy. This removes the need to setup
   SSL certificates for each backend.
2. The backends do not need to be exposed to incoming requests from the proxy.
   In particular, the backends can run in different networks without needing
   to expose them to incoming traffic from the internet.

## Disclaimer

[This is not an official Google product](https://developers.google.com/open-source/projects#some-projects-say-that-they-are-not-an-official-google-product--what------does-that-mean)

## Background

A [reverse proxy](https://en.wikipedia.org/wiki/Reverse_proxy) is a server that
forwards requests to one or more backend servers.

Traditionally, this has been done by configuring the reverse proxy with the
address of each backend, and then having it send a request to a backend for
each request it receives from a client.

That meant each backend had to be able to receive incoming requests from the
proxy. This implied one of two choices.

1. Place the backend servers on a private network shared with the proxy.
2. Place the backend servers on the public internet.

The first choice requires the proxy and backends to be controlled by the same
hosting provider, while the second requires the backends handle all of the
same overhead required for any internet hosting (SSL termination, protection
against DDoS attacks, etc).

This project aims to limit the overhead of hosting a public server to just
the proxy while still allowing anyone to run a backend.

The details of how this is accomplished are outlined below.

## Design

This project defines two components:

1. Something that we are calling the "Inverting Proxy"
2. An agent that runs alongside a backend and forwards requests to it

The inverting proxy can be run on App Engine. It requires incoming requests be
authenticated via the [Users API](https://cloud.google.com/appengine/docs/python/users/)
and forwards those requests to the appropriate backend, via the agent.

Since neither the backend nor the agent will be accessible from the public
internet, requests are not directly forwarded to the backend. Instead,
the direction of that traffic is inverted. The agent sends a request to the
inverting proxy, the proxy waits for incoming client requests, and
then the proxy responds to the request from the agent with those client
requests in the response body.

The agent then forwards those requests to the backend, takes the responses
that it gets back, and sends them as a request to the inverting proxy.

Finally, the inverting proxy extracts the backend's response from the
agent's request and sends it as the response to the original client
request.

An example request flow looks like this:

    +--------+         +-------+         +-------+         +---------+
    | Client |         | Proxy | <-(1)-- | Agent |         | Backend |
    |        | --(2)-> |       |         |       |         |         |
    |        |         |       | --(3)-> |       |         |         |
    |        |         |       |         |       | --(4)-> |         |
    |        |         |       |         |       | <-(5)-- |         |
    |        |         |       | <-(6)-- |       |         |         |
    |        | <-(7)-- |       |         |       |         |         |
    |        |         |       | --(8)-> |       |         |         |
    +--------+         +-------+         +-------+         +---------+

The important thing to note is that the request from the client (#2) matches
the request to the backend (#4) and the response from the backend (#5) matches
the response to the client (#7).

## Components

There are two components required for the inverting proxy to work:

1. The proxy itself, which must be accessible from the public internet.
2. The forwarding agent, which must be able to forward requests to the backend.

### Inverting Proxy

The inverting proxy serves the same roll as a reverse proxy, but additionally
inverts the direction of traffic to the agent.

We implement the inverting proxy as an App Engine app. The app is written in
Go and its source code is under the 'app' subdirectory.

### Forwarding Agent

The agent receives the inverted traffic from the proxy and inverts it again
before forwarding it to the backend. This ensures that the requests from the
client match the requests to the backend and that the responses from the
backend match the responses to the client.

The forwarding agent is standalone binary written in Go and its source code
is under the 'agent' subdirectory.

## Limitations

Currently, the inverting proxy only supports HTTP requests. In particular,
websockets are not supported, so you have to use an adapter like socket.io
if you want to use the inverting proxy with a service that requires websockets.
