package ss

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"net"
	"time"

	"github.com/go-gost/gosocks5"
	"github.com/go-gost/gost/pkg/bypass"
	"github.com/go-gost/gost/pkg/chain"
	"github.com/go-gost/gost/pkg/common/util/ss"
	"github.com/go-gost/gost/pkg/handler"
	"github.com/go-gost/gost/pkg/logger"
	md "github.com/go-gost/gost/pkg/metadata"
	"github.com/go-gost/gost/pkg/registry"
)

func init() {
	registry.RegisterHandler("ss", NewHandler)
}

type ssHandler struct {
	chain  *chain.Chain
	bypass bypass.Bypass
	logger logger.Logger
	md     metadata
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := &handler.Options{}
	for _, opt := range opts {
		opt(options)
	}

	return &ssHandler{
		chain:  options.Chain,
		bypass: options.Bypass,
		logger: options.Logger,
	}
}

func (h *ssHandler) Init(md md.Metadata) (err error) {
	return h.parseMetadata(md)
}

func (h *ssHandler) Handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	start := time.Now()
	h.logger = h.logger.WithFields(map[string]interface{}{
		"remote": conn.RemoteAddr().String(),
		"local":  conn.LocalAddr().String(),
	})

	h.logger.Infof("%s <> %s", conn.RemoteAddr(), conn.LocalAddr())
	defer func() {
		h.logger.WithFields(map[string]interface{}{
			"duration": time.Since(start),
		}).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	// standard UDP relay.
	if pc, ok := conn.(net.PacketConn); ok {
		if h.md.enableUDP {
			h.handleUDP(ctx, conn.RemoteAddr(), pc)
			return
		} else {
			h.logger.Error("UDP relay is diabled")
		}

		return
	}

	if h.md.cipher != nil {
		conn = ss.ShadowConn(h.md.cipher.StreamConn(conn), nil)
	}

	if h.md.readTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(h.md.readTimeout))
	}

	br := bufio.NewReader(conn)
	data, err := br.Peek(3)
	if err != nil {
		h.logger.Error(err)
		h.discard(conn)
		return
	}
	conn.SetReadDeadline(time.Time{})

	conn = handler.NewBufferReaderConn(conn, br)
	if data[2] == 0xff {
		if h.md.enableUDP {
			// UDP-over-TCP relay
			h.handleUDPTun(ctx, conn)
		} else {
			h.logger.Error("UDP relay is diabled")
		}
		return
	}

	// standard TCP.
	addr := &gosocks5.Addr{}
	if _, err = addr.ReadFrom(conn); err != nil {
		h.logger.Error(err)
		h.discard(conn)
		return
	}

	conn.SetReadDeadline(time.Time{})

	h.logger = h.logger.WithFields(map[string]interface{}{
		"dst": addr.String(),
	})

	h.logger.Infof("%s >> %s", conn.RemoteAddr(), addr)

	if h.bypass != nil && h.bypass.Contains(addr.String()) {
		h.logger.Info("bypass: ", addr.String())
		return
	}

	r := (&chain.Router{}).
		WithChain(h.chain).
		WithRetry(h.md.retryCount).
		WithLogger(h.logger)
	cc, err := r.Dial(ctx, "tcp", addr.String())
	if err != nil {
		return
	}
	defer cc.Close()

	t := time.Now()
	h.logger.Infof("%s <-> %s", conn.RemoteAddr(), addr)
	handler.Transport(conn, cc)
	h.logger.
		WithFields(map[string]interface{}{
			"duration": time.Since(t),
		}).
		Infof("%s >-< %s", conn.RemoteAddr(), addr)
}

func (h *ssHandler) discard(conn net.Conn) {
	io.Copy(ioutil.Discard, conn)
}
