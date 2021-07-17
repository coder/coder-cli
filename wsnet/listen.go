package wsnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/pion/webrtc/v3"
	"golang.org/x/net/proxy"
	"nhooyr.io/websocket"

	"cdr.dev/coder-cli/coder-sdk"
)

// Codes for DialChannelResponse.
const (
	CodeDialErr       = "dial_error"
	CodePermissionErr = "permission_error"
	CodeBadAddressErr = "bad_address_error"
)

var connectionRetryInterval = time.Second

// DialChannelResponse is used to notify a dial channel of a
// listening state. Modeled after net.OpError, and marshalled
// to that if Net is not "".
type DialChannelResponse struct {
	Code string
	Err  string
	// Fields are set if the code is CodeDialErr.
	Net string
	Op  string
}

// Listen connects to the broker proxies connections to the local net.
// Close will end all RTC connections.
func Listen(ctx context.Context, broker string, turnProxyAuthToken string) (io.Closer, error) {
	l := &listener{
		broker:             broker,
		connClosers:        make([]io.Closer, 0),
		turnProxyAuthToken: turnProxyAuthToken,
	}
	// We do a one-off dial outside of the loop to ensure the initial
	// connection is successful. If not, there's likely an error the
	// user needs to act on.
	ch, err := l.dial(ctx)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			err := <-ch
			if errors.Is(err, io.EOF) || errors.Is(err, yamux.ErrKeepAliveTimeout) {
				// If we hit an EOF, then the connection to the broker
				// was interrupted. We'll take a short break then dial
				// again.
				ticker := time.NewTicker(connectionRetryInterval)
				for {
					select {
					case <-ticker.C:
						ch, err = l.dial(ctx)
					case <-ctx.Done():
						err = ctx.Err()
					}
					if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						break
					}
				}
				ticker.Stop()
			}
			if err != nil {
				l.acceptError = err
				_ = l.Close()
				break
			}
		}
	}()
	return l, nil
}

type listener struct {
	broker             string
	turnProxyAuthToken string

	acceptError    error
	ws             *websocket.Conn
	connClosers    []io.Closer
	connClosersMut sync.Mutex
}

func (l *listener) dial(ctx context.Context) (<-chan error, error) {
	if l.ws != nil {
		_ = l.ws.Close(websocket.StatusNormalClosure, "new connection inbound")
	}
	conn, resp, err := websocket.Dial(ctx, l.broker, nil)
	if err != nil {
		if resp != nil {
			return nil, coder.NewHTTPError(resp)
		}
		return nil, err
	}
	l.ws = conn
	nconn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	config := yamux.DefaultConfig()
	config.LogOutput = io.Discard
	session, err := yamux.Server(nconn, config)
	if err != nil {
		return nil, fmt.Errorf("create multiplex: %w", err)
	}
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		for {
			conn, err := session.Accept()
			if err != nil {
				errCh <- err
				break
			}
			go l.negotiate(conn)
		}
	}()
	return errCh, nil
}

// Negotiates the handshake protocol over the connection provided.
// This functions control-flow is important to readability,
// so the cognitive overload linter has been disabled.
// nolint:gocognit,nestif
func (l *listener) negotiate(conn net.Conn) {
	var (
		err     error
		decoder = json.NewDecoder(conn)
		rtc     *webrtc.PeerConnection
		// If candidates are sent before an offer, we place them here.
		// We currently have no assurances to ensure this can't happen,
		// so it's better to buffer and process than fail.
		pendingCandidates = []webrtc.ICECandidateInit{}
		// Sends the error provided then closes the connection.
		// If RTC isn't connected, we'll close it.
		closeError = func(err error) {
			d, _ := json.Marshal(&BrokerMessage{
				Error: err.Error(),
			})
			_, _ = conn.Write(d)
			_ = conn.Close()
			if rtc != nil {
				if rtc.ConnectionState() != webrtc.PeerConnectionStateConnected {
					rtc.Close()
					rtc = nil
				}
			}
		}
	)

	for {
		var msg BrokerMessage
		err = decoder.Decode(&msg)
		if err != nil {
			closeError(err)
			return
		}

		if msg.Candidate != "" {
			c := webrtc.ICECandidateInit{
				Candidate: msg.Candidate,
			}

			if rtc == nil {
				pendingCandidates = append(pendingCandidates, c)
				continue
			}

			err = rtc.AddICECandidate(c)
			if err != nil {
				closeError(fmt.Errorf("accept ice candidate: %w", err))
				return
			}
		}

		if msg.Offer != nil {
			if msg.Servers == nil {
				closeError(fmt.Errorf("ICEServers must be provided"))
				return
			}
			for _, server := range msg.Servers {
				if server.Username == turnProxyMagicUsername {
					// This candidate is only used when proxying,
					// so it will not validate.
					continue
				}
				err = DialICE(server, nil)
				if err != nil {
					closeError(fmt.Errorf("dial server %+v: %w", server.URLs, err))
					return
				}
			}
			var turnProxy proxy.Dialer
			if msg.TURNProxyURL != "" {
				u, err := url.Parse(msg.TURNProxyURL)
				if err != nil {
					closeError(fmt.Errorf("parse turn proxy url: %w", err))
					return
				}
				turnProxy = &turnProxyDialer{
					baseURL: u,
					token:   l.turnProxyAuthToken,
				}
			}
			rtc, err = newPeerConnection(msg.Servers, turnProxy)
			if err != nil {
				closeError(err)
				return
			}
			rtc.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
				if pcs == webrtc.PeerConnectionStateConnecting {
					return
				}
				_ = conn.Close()
			})
			flushCandidates := proxyICECandidates(rtc, conn)
			l.connClosersMut.Lock()
			l.connClosers = append(l.connClosers, rtc)
			l.connClosersMut.Unlock()
			rtc.OnDataChannel(l.handle(msg))
			err = rtc.SetRemoteDescription(*msg.Offer)
			if err != nil {
				closeError(fmt.Errorf("apply offer: %w", err))
				return
			}
			answer, err := rtc.CreateAnswer(nil)
			if err != nil {
				closeError(fmt.Errorf("create answer: %w", err))
				return
			}
			err = rtc.SetLocalDescription(answer)
			if err != nil {
				closeError(fmt.Errorf("set local answer: %w", err))
				return
			}
			flushCandidates()

			data, err := json.Marshal(&BrokerMessage{
				Answer: rtc.LocalDescription(),
			})
			if err != nil {
				closeError(fmt.Errorf("marshal: %w", err))
				return
			}
			_, err = conn.Write(data)
			if err != nil {
				closeError(fmt.Errorf("write: %w", err))
				return
			}

			for _, candidate := range pendingCandidates {
				err = rtc.AddICECandidate(candidate)
				if err != nil {
					closeError(fmt.Errorf("add pending candidate: %w", err))
					return
				}
			}
			pendingCandidates = nil
		}
	}
}

func (l *listener) handle(msg BrokerMessage) func(dc *webrtc.DataChannel) {
	return func(dc *webrtc.DataChannel) {
		if dc.Protocol() == controlChannel {
			// The control channel handles pings.
			dc.OnOpen(func() {
				rw, err := dc.Detach()
				if err != nil {
					return
				}
				// We'll read and write back a single byte for ping/pongin'.
				d := make([]byte, 1)
				for {
					_, err = rw.Read(d)
					if errors.Is(err, io.EOF) {
						return
					}
					if err != nil {
						continue
					}
					_, _ = rw.Write(d)
				}
			})
			return
		}

		dc.OnOpen(func() {
			rw, err := dc.Detach()
			if err != nil {
				return
			}

			var init DialChannelResponse
			sendInitMessage := func() {
				initData, err := json.Marshal(&init)
				if err != nil {
					rw.Close()
					return
				}
				_, err = rw.Write(initData)
				if err != nil {
					return
				}
				if init.Err != "" {
					// If an error occurred, we're safe to close the connection.
					dc.Close()
					return
				}
			}

			network, addr, err := msg.getAddress(dc.Protocol())
			if err != nil {
				init.Code = CodeBadAddressErr
				init.Err = err.Error()
				var policyErr notPermittedByPolicyErr
				if errors.As(err, &policyErr) {
					init.Code = CodePermissionErr
				}
				sendInitMessage()
				return
			}

			nc, err := net.Dial(network, addr)
			if err != nil {
				init.Code = CodeDialErr
				init.Err = err.Error()
				if op, ok := err.(*net.OpError); ok {
					init.Net = op.Net
					init.Op = op.Op
				}
			}
			sendInitMessage()
			if init.Err != "" {
				return
			}
			// Must wrap the data channel inside this connection
			// for buffering from the dialed endpoint to the client.
			co := &dataChannelConn{
				addr: nil,
				dc:   dc,
				rw:   rw,
			}
			co.init()
			defer nc.Close()
			defer co.Close()
			go func() {
				defer dc.Close()
				_, _ = io.Copy(co, nc)
			}()
			_, _ = io.Copy(nc, co)
		})
	}
}

// Close closes the broker socket and all created RTC connections.
func (l *listener) Close() error {
	l.connClosersMut.Lock()
	for _, connCloser := range l.connClosers {
		// We can ignore the error here... it doesn't
		// really matter if these fail to close.
		_ = connCloser.Close()
	}
	l.connClosersMut.Unlock()
	return l.ws.Close(websocket.StatusNormalClosure, "")
}

// Since this listener is bound to the WebSocket, we could
// return that resolved Addr, but until we need it we won't.
func (l *listener) Addr() net.Addr {
	return nil
}
