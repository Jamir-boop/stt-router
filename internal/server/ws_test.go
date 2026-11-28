package server

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

// wsPair returns a connected client and server wsConn over loopback TCP.
func wsPair(t *testing.T) (client, server *wsConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc := <-accepted
	t.Cleanup(func() { cc.Close(); sc.Close() })
	return &wsConn{rwc: cc, br: bufio.NewReader(cc), client: true}, &wsConn{rwc: sc, br: bufio.NewReader(sc)}
}

func TestWSAccept(t *testing.T) {
	if got := wsAccept("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept %q", got)
	}
}

func TestWSMaskedTextRoundTrip(t *testing.T) {
	client, server := wsPair(t)
	if err := client.write(opText, []byte(`{"type":"x"}`)); err != nil {
		t.Fatal(err)
	}
	op, msg, err := server.readMessage()
	if err != nil || op != opText || string(msg) != `{"type":"x"}` {
		t.Fatalf("server got op %d %q err %v", op, msg, err)
	}
	if err := server.write(opText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	op, msg, err = client.readMessage()
	if err != nil || op != opText || string(msg) != "hello" {
		t.Fatalf("client got op %d %q err %v", op, msg, err)
	}
}

func TestWSFragmentedMessage(t *testing.T) {
	client, server := wsPair(t)
	if err := client.writeFrame(opText, false, []byte(`{"ty`)); err != nil {
		t.Fatal(err)
	}
	if err := client.writeFrame(opContinuation, false, []byte(`pe":`)); err != nil {
		t.Fatal(err)
	}
	if err := client.writeFrame(opContinuation, true, []byte(`"y"}`)); err != nil {
		t.Fatal(err)
	}
	op, msg, err := server.readMessage()
	if err != nil || op != opText || string(msg) != `{"type":"y"}` {
		t.Fatalf("got op %d %q err %v", op, msg, err)
	}
}

func TestWSPingPong(t *testing.T) {
	client, server := wsPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = server.readMessage() // answers the ping, then waits for a data message
	}()
	if err := client.write(opPing, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	f, err := client.readFrame()
	if err != nil || f.op != opPong || string(f.pay) != "hi" {
		t.Fatalf("pong: %+v err %v", f, err)
	}
	client.write(opClose, nil)
	<-done
}

func TestWSOversizeMessage(t *testing.T) {
	client, server := wsPair(t)
	hdr := []byte{0x81, 0x80 | 127}
	hdr = binary.BigEndian.AppendUint64(hdr, 2<<20)
	hdr = append(hdr, 0, 0, 0, 0) // a zero mask, the payload is never read
	if _, err := client.rwc.Write(hdr); err != nil {
		t.Fatal(err)
	}
	var we *wsError
	if _, _, err := server.readMessage(); !errors.As(err, &we) || we.code != 1009 {
		t.Fatalf("err %v, want close code 1009", err)
	}
}

func TestWSRejectsUnmaskedClientFrame(t *testing.T) {
	client, server := wsPair(t)
	// A client frame without the mask bit that RFC 6455 requires.
	if _, err := client.rwc.Write([]byte{0x81, 0x01, 'x'}); err != nil {
		t.Fatal(err)
	}
	var we *wsError
	if _, _, err := server.readMessage(); !errors.As(err, &we) || we.code != 1002 {
		t.Fatalf("err %v, want close code 1002", err)
	}
}
