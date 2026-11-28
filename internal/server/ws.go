package server

// A small RFC 6455 WebSocket endpoint: handshake, frame codec, fragmentation,
// ping and close. No extensions and no compression.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 1 << 20 // one client message; audio appends are much smaller
	wsWriteWait  = 30 * time.Second
)

// WebSocket opcodes.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xa
)

var errWSClosed = errors.New("websocket: connection closed")

// wsError is a protocol error that is reported to the peer in a close frame.
type wsError struct {
	code int
	msg  string
}

func (e *wsError) Error() string { return e.msg }

// wsConn is one end of a WebSocket connection. A server reads masked frames
// and writes plain frames; client flips that for tests.
type wsConn struct {
	rwc    net.Conn
	br     *bufio.Reader
	client bool
	rd     time.Duration // read deadline per frame; 0 disables it
	wmu    sync.Mutex
}

type wsFrame struct {
	op  byte
	fin bool
	pay []byte
}

// headerHas reports whether a comma separated header has the token.
func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsUpgrade answers the handshake and returns the hijacked connection and its
// buffered reader, which can hold frames the client sent with the request.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the connection cannot be hijacked")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		wsAccept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw.Reader, nil
}

// write sends one data frame.
func (c *wsConn) write(op byte, pay []byte) error {
	return c.writeFrame(op, true, pay)
}

// writeFrame sends one frame. A client masks its frames, as RFC 6455 requires.
func (c *wsConn) writeFrame(op byte, fin bool, pay []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(op, fin, pay)
}

func (c *wsConn) writeFrameLocked(op byte, fin bool, pay []byte) error {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	h := make([]byte, 0, 14)
	h = append(h, b0)
	var ext [8]byte
	switch n := len(pay); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xffff:
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		h = append(h, 126, ext[0], ext[1])
	default:
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		h = append(h, 127)
		h = append(h, ext[:]...)
	}
	body := pay
	if c.client {
		h[1] |= 0x80
		var key [4]byte
		_, _ = rand.Read(key[:])
		h = append(h, key[:]...)
		body = make([]byte, len(pay))
		for i := range pay {
			body[i] = pay[i] ^ key[i&3]
		}
	}
	c.rwc.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if _, err := c.rwc.Write(h); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := c.rwc.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// closeWith sends a close frame. A failed write means the peer is already gone.
func (c *wsConn) closeWith(code int) {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(code))
	c.write(opClose, p[:])
}

// readFrame reads one frame and unmasks its payload.
func (c *wsConn) readFrame() (wsFrame, error) {
	if c.rd > 0 {
		c.rwc.SetReadDeadline(time.Now().Add(c.rd))
	}
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return wsFrame{}, err
	}
	if h[0]&0x70 != 0 {
		return wsFrame{}, &wsError{1002, "reserved bits are not supported"}
	}
	op := h[0] & 0x0f
	fin := h[0]&0x80 != 0
	masked := h[1]&0x80 != 0
	if masked == c.client {
		return wsFrame{}, &wsError{1002, "wrong frame masking"}
	}
	n := int64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return wsFrame{}, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return wsFrame{}, err
		}
		if u := binary.BigEndian.Uint64(b[:]); u > wsMaxMessage {
			return wsFrame{}, &wsError{1009, "message too big"}
		} else {
			n = int64(u)
		}
	}
	switch {
	case op >= opClose:
		if !fin || n > 125 || (op != opClose && op != opPing && op != opPong) {
			return wsFrame{}, &wsError{1002, "bad control frame"}
		}
	case op > opBinary:
		return wsFrame{}, &wsError{1002, "reserved opcode"}
	case n > wsMaxMessage:
		return wsFrame{}, &wsError{1009, "message too big"}
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return wsFrame{}, err
		}
	}
	pay := make([]byte, n)
	if _, err := io.ReadFull(c.br, pay); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range pay {
			pay[i] ^= key[i&3]
		}
	}
	return wsFrame{op: op, fin: fin, pay: pay}, nil
}

// readMessage returns the next text or binary message. It handles control
// frames, continuation frames and the message size limit.
func (c *wsConn) readMessage() (byte, []byte, error) {
	var op byte
	var buf []byte
	for {
		f, err := c.readFrame()
		if err != nil {
			var we *wsError
			if errors.As(err, &we) {
				c.closeWith(we.code)
			}
			return 0, nil, err
		}
		switch f.op {
		case opPing:
			if err := c.write(opPong, f.pay); err != nil {
				return 0, nil, err
			}
		case opPong:
		case opClose:
			c.write(opClose, f.pay) // echo the close handshake
			return 0, nil, errWSClosed
		case opContinuation:
			if op == 0 {
				c.closeWith(1002)
				return 0, nil, &wsError{1002, "unexpected continuation frame"}
			}
			buf = append(buf, f.pay...)
			if len(buf) > wsMaxMessage {
				c.closeWith(1009)
				return 0, nil, &wsError{1009, "message too big"}
			}
			if f.fin {
				return op, buf, nil
			}
		case opText, opBinary:
			if op != 0 {
				c.closeWith(1002)
				return 0, nil, &wsError{1002, "new message inside a fragmented message"}
			}
			op, buf = f.op, f.pay
			if f.fin {
				return op, buf, nil
			}
		}
	}
}
