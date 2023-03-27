// Package iap provides a client for the IAP tunneling protocol.
package iap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"nhooyr.io/websocket"
)

const (
	proxySubproto = "relay.tunnel.cloudproxy.app"
	proxyHost     = "tunnel.cloudproxy.app"
	proxyPath     = "/v4/connect"
	proxyOrigin   = "bot:iap-tunneler"
)

const (
	subprotoMaxFrameSize        = 16384
	subprotoTagSuccess   uint16 = 0x1
	subprotoTagData      uint16 = 0x4
	subprotoTagAck       uint16 = 0x7
)

func min[T int | uint](a, b T) T {
	if a < b {
		return a
	}
	return b
}

// copyNBuffer is like io.CopyN but stages through a given buffer like io.CopyBuffer.
func copyNBuffer(w io.Writer, r io.Reader, n int64, buf []byte) (int64, error) {
	return io.CopyBuffer(w, io.LimitReader(r, n), buf)
}

type Conn struct {
	conn      *websocket.Conn
	connected bool
	sessionID []byte

	recvNbAcked   uint64
	recvNbUnacked uint64
	recvBuf       []byte
	recvReader    *io.PipeReader
	recvWriter    *io.PipeWriter

	sendNbAcked   uint64
	sendNbUnacked uint64
	sendNbCh      chan int
	sendBuf       []byte
	sendReader    *io.PipeReader
	sendWriter    *io.PipeWriter
}

func connectURL(dopts *dialOptions) string {
	query := url.Values{
		"zone":      []string{dopts.Zone},
		"region":    []string{dopts.Region},
		"project":   []string{dopts.Project},
		"port":      []string{dopts.Port},
		"network":   []string{dopts.Network},
		"interface": []string{dopts.Interface},
		"instance":  []string{dopts.Instance},
		"host":      []string{dopts.Host},
		"group":     []string{dopts.Group},
	}

	for key, value := range query {
		if value[0] == "" {
			query.Del(key)
		}
	}

	url := url.URL{
		Scheme:   "wss",
		Host:     proxyHost,
		Path:     proxyPath,
		RawQuery: query.Encode(),
	}

	return url.String()
}

// Dial connects to the IAP proxy and returns a Conn or error if the connection fails.
func Dial(ctx context.Context, opts ...DialOption) (*Conn, error) {
	dopts := &dialOptions{}
	dopts.collectOpts(opts)

	wsOptions := websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{fmt.Sprintf("Bearer %v", dopts.Token)},
			"Origin":        []string{proxyOrigin},
		},
		Subprotocols:    []string{proxySubproto},
		CompressionMode: websocket.CompressionDisabled,
	}
	if dopts.Compress {
		wsOptions.CompressionMode = websocket.CompressionContextTakeover
	}

	conn, _, err := websocket.Dial(ctx, connectURL(dopts), &wsOptions)
	if err != nil {
		return nil, err
	}

	recvReader, recvWriter := io.Pipe()
	sendReader, sendWriter := io.Pipe()

	c := &Conn{
		conn: conn,

		recvBuf:    make([]byte, subprotoMaxFrameSize),
		recvReader: recvReader,
		recvWriter: recvWriter,

		sendNbCh:   make(chan int),
		sendBuf:    make([]byte, subprotoMaxFrameSize),
		sendReader: sendReader,
		sendWriter: sendWriter,
	}
	if err := c.readFrame(); err != nil {
		return nil, err
	}

	go c.read()
	go c.write()

	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error {
	close(c.sendNbCh)
	return c.conn.Close(websocket.StatusNormalClosure, "Connection closed")
}

// Read reads data from the connection.
func (c *Conn) Read(buf []byte) (n int, err error) {
	return c.recvReader.Read(buf)
}

// Write writes data to the connection.
func (c *Conn) Write(buf []byte) (n int, err error) {
	c.sendNbCh <- len(buf)
	return c.sendWriter.Write(buf)
}

// SessionID returns the session ID of the connection.
func (c *Conn) SessionID() string {
	return string(c.sessionID)
}

// Sent returns the number of bytes sent and acked.
func (c *Conn) Sent() uint64 {
	return c.sendNbAcked
}

// Received returns the number of bytes received and acked.
func (c *Conn) Received() uint64 {
	return c.recvNbAcked
}

func (c *Conn) writeAck(bytes uint64) error {
	writer, err := c.conn.Writer(context.Background(), websocket.MessageBinary)
	if err != nil {
		return err
	}
	defer writer.Close()

	binary.Write(writer, binary.BigEndian, subprotoTagAck)
	binary.Write(writer, binary.BigEndian, bytes)

	return nil
}

func (c *Conn) readSuccessFrame(buf [8]byte, r io.Reader) error {
	if _, err := r.Read(buf[:4]); err != nil {
		return err
	}
	len := binary.BigEndian.Uint32(buf[:4])
	if len > subprotoMaxFrameSize {
		return errors.New("len exceeds subprotocol max data frame size")
	}

	c.sessionID = make([]byte, len)
	if _, err := r.Read([]byte(c.sessionID)); err != nil {
		return err
	}

	c.connected = true
	return nil
}

func (c *Conn) readAckFrame(buf [8]byte, r io.Reader) error {
	if _, err := r.Read(buf[:8]); err != nil {
		return err
	}

	// TODO: should we transmit?
	// since it's over TCP this seems redundant

	c.sendNbAcked = binary.BigEndian.Uint64(buf[:8])
	return nil
}

func (c *Conn) readDataFrame(buf [8]byte, r io.Reader) error {
	if _, err := r.Read(buf[:4]); err != nil {
		return err
	}
	len := binary.BigEndian.Uint32(buf[:4])
	if len > subprotoMaxFrameSize {
		return errors.New("len exceeds subprotocol max data frame size")
	}

	if _, err := copyNBuffer(c.recvWriter, r, int64(len), c.recvBuf); err != nil {
		return err
	}
	c.recvNbUnacked += uint64(len)

	return nil
}

func (c *Conn) readFrame() error {
	buf := [8]byte{}

	_, reader, err := c.conn.Reader(context.Background())
	if err != nil {
		var closeError websocket.CloseError
		if errors.As(err, &closeError) {
			return fmt.Errorf("Proxy closed connection with code %v, reason: %v", int(closeError.Code), closeError.Reason)
		}
		return err
	}

	if _, err := reader.Read(buf[:2]); err != nil {
		return err
	}
	tag := binary.BigEndian.Uint16(buf[:2])

	switch tag {
	case subprotoTagSuccess:
		err = c.readSuccessFrame(buf, reader)
	default:
		if !c.connected {
			return fmt.Errorf("Received frame before connection was established")
		}

		switch tag {
		case subprotoTagAck:
			err = c.readAckFrame(buf, reader)
		case subprotoTagData:
			err = c.readDataFrame(buf, reader)

			// can the threshold be increased?
			if c.recvNbUnacked-c.recvNbAcked > 2*subprotoMaxFrameSize {
				if err := c.writeAck(c.recvNbUnacked); err != nil {
					return err
				}
				c.recvNbAcked = c.recvNbUnacked
			}
		default:
			// unknown tags should be ignored
			return nil
		}

	}

	return err
}

func (c *Conn) writeFrame() error {
	nb, ok := <-c.sendNbCh
	if !ok {
		return io.EOF
	}

	for nb > 0 {
		// clamp each write to max frame size
		nbLimit := min(nb, subprotoMaxFrameSize)

		writer, err := c.conn.Writer(context.Background(), websocket.MessageBinary)
		if err != nil {
			return err
		}

		binary.Write(writer, binary.BigEndian, subprotoTagData)
		binary.Write(writer, binary.BigEndian, uint32(nbLimit))

		if _, err := copyNBuffer(writer, c.sendReader, int64(nbLimit), c.sendBuf); err != nil {
			return err
		}
		writer.Close()

		c.sendNbUnacked += uint64(nbLimit)
		nb -= nbLimit
	}

	return nil
}

func (c *Conn) read() {
	for {
		if err := c.readFrame(); err != nil {
			break
		}
	}
}

func (c *Conn) write() {
	for {
		if err := c.writeFrame(); err != nil {
			break
		}
	}
}
