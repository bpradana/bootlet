package main

import (
	"context"

	"github.com/coder/websocket"

	"bootlet/emulator/netstack"
)

// relayLink carries a WispUpstream's packets over a WebSocket to a relay.
// The upstream is only touched from the main loop (in pump); the reader
// goroutine hands packets over through a channel.
type relayLink struct {
	up   *netstack.WispUpstream
	ws   *websocket.Conn
	in   chan []byte
	wake chan struct{}
	lost bool
}

func dialRelay(url string, up *netstack.WispUpstream) (*relayLink, error) {
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	r := &relayLink{up: up, ws: ws, in: make(chan []byte, 1024), wake: make(chan struct{}, 1)}
	go func() {
		defer close(r.in)
		for {
			_, msg, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			r.in <- msg
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}
	}()
	return r, nil
}

// pump delivers received packets to the upstream and sends its output.
func (r *relayLink) pump() {
	for {
		select {
		case pkt, ok := <-r.in:
			if !ok {
				if !r.lost {
					r.lost = true
					r.up.Disconnected()
				}
				r.in = nil
				continue
			}
			r.up.Input(pkt)
			continue
		default:
		}
		break
	}
	for _, pkt := range r.up.Output() {
		if r.lost || r.ws.Write(context.Background(), websocket.MessageBinary, pkt) != nil {
			return
		}
	}
}
