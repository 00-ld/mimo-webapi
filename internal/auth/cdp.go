package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cdpClient is a minimal Chrome DevTools Protocol client.
//
// It speaks the protocol directly over a WebSocket rather than pulling in a
// websocket dependency: the relay ships as a single static binary and one
// small framed-protocol implementation is a better trade than a module tree.
type cdpClient struct {
	conn net.Conn
	br   *bufio.Reader
	next int
}

// dial opens a DevTools connection to the first page target the browser has.
func (f *Flow) dial(ctx context.Context) (*cdpClient, error) {
	wsURL, err := f.pageTarget(ctx)
	if err != nil {
		return nil, err
	}
	return dialWebSocket(ctx, wsURL)
}

// pageTarget finds a debuggable page.
//
// Chrome's DevTools HTTP endpoint only lists targets after the browser has
// finished starting, so this retries briefly rather than failing the poll. The
// retry window is short and capped by the caller's context: WaitForLogin polls
// this once a second, and a long internal retry here would swallow the
// caller's own deadline.
func (f *Flow) pageTarget(ctx context.Context) (string, error) {
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var lastErr error
	for {
		targets, err := f.listTargets(ctx)
		if err == nil && len(targets) > 0 {
			return targets[0], nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("the browser has no open page")
		}
		if time.Now().After(deadline) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// listTargets asks the browser for its debuggable pages.
func (f *Flow) listTargets(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/list", f.port), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("the browser is not accepting DevTools connections yet")
	}
	defer resp.Body.Close()

	var list []struct {
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode target list: %w", err)
	}
	var out []string
	for _, t := range list {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			out = append(out, t.WebSocketDebuggerURL)
		}
	}
	return out, nil
}

// dialWebSocket performs the RFC 6455 handshake.
func dialWebSocket(ctx context.Context, wsURL string) (*cdpClient, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("connect to browser: %w", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	path := u.Path
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("browser rejected the WebSocket handshake: %s",
			strings.TrimSpace(statusLine))
	}
	// Consume headers, verifying the accept token.
	var accept string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(v)
		}
	}
	if accept != "" && accept != wsAccept(key) {
		conn.Close()
		return nil, errors.New("browser returned an invalid WebSocket accept token")
	}

	return &cdpClient{conn: conn, br: br}, nil
}

// wsAccept computes the Sec-WebSocket-Accept response for a key.
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// call sends one CDP command and waits for its result.
func (c *cdpClient) call(method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(body); err != nil {
		return nil, err
	}

	// Events may interleave with the reply; skip anything without our id.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s", method)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		frame, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame, &reply); err != nil {
			continue
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// writeFrame sends a masked client text frame.
func (c *cdpClient) writeFrame(payload []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	var hdr []byte
	hdr = append(hdr, 0x81) // FIN + text
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)

	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// readFrame reads one complete frame, following continuation frames.
func (c *cdpClient) readFrame() ([]byte, error) {
	var out []byte
	for {
		h, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		fin := h&0x80 != 0
		opcode := h & 0x0f

		lengthByte, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		masked := lengthByte&0x80 != 0
		n := uint64(lengthByte & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return nil, err
			}
		}

		// Guard against a hostile or broken peer claiming a huge frame.
		if n > 32<<20 {
			return nil, errors.New("browser sent an implausibly large frame")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}

		switch opcode {
		case 0x1, 0x2, 0x0: // text, binary, continuation
			out = append(out, payload...)
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping
			// Reply with a pong so the browser keeps the socket open.
			_ = c.writeControlFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		}
		if fin {
			return out, nil
		}
	}
}

// writeControlFrame sends a small unmasked control frame.
func (c *cdpClient) writeControlFrame(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	frame := append([]byte{0x80 | opcode, byte(len(payload))}, payload...)
	_, err := c.conn.Write(frame)
	return err
}

func (c *cdpClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
