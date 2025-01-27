package soju

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/time/rate"
	"gopkg.in/irc.v4"
	"nhooyr.io/websocket"
)

// ircConn is a generic IRC connection. It's similar to net.Conn but focuses on
// reading and writing IRC messages.
type ircConn interface {
	ReadMessage() (*irc.Message, error)
	WriteMessage(*irc.Message) error
	Close() error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
}

func newNetIRCConn(c net.Conn) ircConn {
	type netConn net.Conn
	return struct {
		*irc.Conn
		netConn
	}{irc.NewConn(c), c}
}

type websocketIRCConn struct {
	conn                        *websocket.Conn
	readDeadline, writeDeadline time.Time
	remoteAddr                  string
}

func newWebsocketIRCConn(c *websocket.Conn, remoteAddr string) ircConn {
	return &websocketIRCConn{conn: c, remoteAddr: remoteAddr}
}

func (wic *websocketIRCConn) ReadMessage() (*irc.Message, error) {
	ctx := context.Background()
	if !wic.readDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, wic.readDeadline)
		defer cancel()
	}
	_, b, err := wic.conn.Read(ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return nil, io.EOF
		default:
			return nil, err
		}
	}
	return irc.ParseMessage(string(b))
}

func (wic *websocketIRCConn) WriteMessage(msg *irc.Message) error {
	b := []byte(strings.ToValidUTF8(msg.String(), string(unicode.ReplacementChar)))
	ctx := context.Background()
	if !wic.writeDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, wic.writeDeadline)
		defer cancel()
	}
	return wic.conn.Write(ctx, websocket.MessageText, b)
}

func isErrWebSocketClosed(err error) bool {
	return err != nil && strings.HasSuffix(err.Error(), "failed to close WebSocket: already wrote close")
}

func (wic *websocketIRCConn) Close() error {
	err := wic.conn.Close(websocket.StatusNormalClosure, "")
	// TODO: remove once this PR is merged:
	// https://github.com/nhooyr/websocket/pull/303
	if isErrWebSocketClosed(err) {
		return nil
	}
	return err
}

func (wic *websocketIRCConn) SetReadDeadline(t time.Time) error {
	wic.readDeadline = t
	return nil
}

func (wic *websocketIRCConn) SetWriteDeadline(t time.Time) error {
	wic.writeDeadline = t
	return nil
}

func (wic *websocketIRCConn) RemoteAddr() net.Addr {
	return websocketAddr(wic.remoteAddr)
}

func (wic *websocketIRCConn) LocalAddr() net.Addr {
	// Behind a reverse HTTP proxy, we don't have access to the real listening
	// address
	return websocketAddr("")
}

type websocketAddr string

func (websocketAddr) Network() string {
	return "ws"
}

func (wa websocketAddr) String() string {
	return string(wa)
}

type connOptions struct {
	Logger         Logger
	RateLimitDelay time.Duration
	RateLimitBurst int
}

type conn struct {
	conn   ircConn
	srv    *Server
	logger Logger

	lock     sync.Mutex
	outgoing chan<- *irc.Message
	closed   bool
	closedCh chan struct{}
}

func newConn(srv *Server, ic ircConn, options *connOptions) *conn {
	outgoing := make(chan *irc.Message, 64)
	c := &conn{
		conn:     ic,
		srv:      srv,
		outgoing: outgoing,
		logger:   options.Logger,
		closedCh: make(chan struct{}),
	}

	go func() {
		ctx, cancel := c.NewContext(context.Background())
		defer cancel()

		rl := rate.NewLimiter(rate.Every(options.RateLimitDelay), options.RateLimitBurst)
		for msg := range outgoing {
			if err := rl.Wait(ctx); err != nil {
				break
			}

			c.logger.Debugf("sent: %v", msg)
			c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.conn.WriteMessage(msg); err != nil {
				c.logger.Printf("failed to write message: %v", err)
				break
			}
		}
		if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.logger.Printf("failed to close connection: %v", err)
		} else {
			c.logger.Debugf("connection closed")
		}
		// Drain the outgoing channel to prevent SendMessage from blocking
		for range outgoing {
			// This space is intentionally left blank
		}
	}()

	c.logger.Debugf("new connection")
	return c
}

func (c *conn) isClosed() bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.closed
}

// Close closes the connection. It is safe to call from any goroutine.
func (c *conn) Close() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.closed {
		return fmt.Errorf("connection already closed")
	}

	err := c.conn.Close()
	c.closed = true
	close(c.outgoing)
	close(c.closedCh)
	return err
}

func (c *conn) ReadMessage() (*irc.Message, error) {
	msg, err := c.conn.ReadMessage()
	if errors.Is(err, net.ErrClosed) {
		return nil, io.EOF
	} else if err != nil {
		return nil, err
	}

	c.logger.Debugf("received: %v", msg)
	return msg, nil
}

// SendMessage queues a new outgoing message. It is safe to call from any
// goroutine.
//
// If the connection is closed before the message is sent, SendMessage silently
// drops the message.
func (c *conn) SendMessage(ctx context.Context, msg *irc.Message) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.closed {
		return
	}

	select {
	case c.outgoing <- msg:
		// Success
	case <-ctx.Done():
		c.logger.Printf("failed to send message: %v", ctx.Err())
	}
}

func (c *conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// NewContext returns a copy of the parent context with a new Done channel. The
// returned context's Done channel is closed when the connection is closed,
// when the returned cancel function is called, or when the parent context's
// Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
func (c *conn) NewContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	go func() {
		defer cancel()

		select {
		case <-ctx.Done():
			// The parent context has been cancelled, or the caller has called
			// cancel()
		case <-c.closedCh:
			// The connection has been closed
		}
	}()

	return ctx, cancel
}
