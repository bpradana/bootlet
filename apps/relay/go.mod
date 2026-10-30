module bootlet/relay

go 1.27.3

replace bootlet/shared => ../../packages/shared

require (
	bootlet/shared v0.0.0-00010101000000-000000000000
	github.com/coder/websocket v1.8.15
)
