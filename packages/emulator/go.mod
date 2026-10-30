module bootlet/emulator

go 1.27.3

require bootlet/shared v0.0.0-00010101000000-000000000000

require (
	github.com/coder/websocket v1.8.15 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/term v0.47.0 // indirect
)

replace bootlet/shared => ../shared
