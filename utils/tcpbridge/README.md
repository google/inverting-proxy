# TCP-Over-Websocket Bridge

This directory provides a bridge that tunnels TCP connections over Websocket connections.

The purpose of that is to allow non-HTTP traffic to be forwarded over the inverting proxy.

## Usage

This bridge is implemented via two proxies:

1. A frontend that listens for incoming TCP connections and forwards them over a
   Websocket connection.
2. A backend that listens for these incoming websocket connections and forwards the
   information sent over them to a TCP connection to a specified port.

## Installation

```sh
go install github.com/google/inverting-proxy/utils/tcpbridge/...@latest
```

## System Diagram

           +----------+         +-------+         +-------+         +---------+
           | Frontend |         | Proxy |         | Agent |         | Backend |
           |          |         |       |         |       |         |         |
    -TCP-> |          |         |       |         |       |         |         |
           |          |         |       | <-(1)-- |       |         |         |
           |          | --(2)-> |       |         |       |         |         |
           |          |         |       | --(3)-> |       |         |         |
           |          |         |       |         |       | --(4)-> |         |
           |          |         |       |         |       |         |         |
           |          |         |       |         |       |         |         | -TCP->
           |          | <-(7)-> |       | <-(6)-> |       | <-(5)-> |         |
           +----------+         +-------+         +-------+         +---------+

The left-most and right-most arrows represent arbitrary TCP connections, whereas
the numbered arrows in the rest of the diagram represent the following:

1. An HTTP GET request from the agent to list pending requests
2. A websocket upgrade request from the frontend of the TCP-Over-Websocket bridge.
3. An HTTP response from the proxy to the agent.
4. A websocket upgrade request from the proxy agent to the backend of the bridge.
5. Websocket messages sent in both directions between the bridge backend and the agent.
6. HTTP GET and POST messages sent between the agent and proxy containing the contents
   of websocket messages sent in each direction.
7. Websockent messages sent in both directions between the proxy and bridge frontend.
