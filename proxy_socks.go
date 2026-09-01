// SOCKS5 代理服务端：支持无认证与用户名/密码认证（RFC 1929），仅实现 CONNECT 命令。
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKSServer 是一个 SOCKS5 代理服务器。
type SOCKSServer struct {
	// creds 非 nil 时要求用户名/密码认证
	creds func(user, pass string) bool
	// backend 非 nil 时链经该上游代理
	backend *ProxyRoute
	// Dial 自定义建连（测试注入）
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// newSOCKSServer 创建 SOCKS5 服务器。
func newSOCKSServer(creds func(u, p string) bool, backend *ProxyRoute) *SOCKSServer {
	return &SOCKSServer{creds: creds, backend: backend}
}

// Serve 在 listener 上接受连接并处理。
func (s *SOCKSServer) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *SOCKSServer) handleConn(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 握手
	br := newBufConn(conn)
	if err := s.negotiate(ctx, br); err != nil {
		return
	}
	if err := s.handleRequest(ctx, br); err != nil {
		return
	}
}

// negotiate 完成版本协商与认证。
func (s *SOCKSServer) negotiate(ctx context.Context, c *bufConn) error {
	// VER, NMETHODS, METHODS
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return errors.New("unsupported socks version")
	}
	nmethods := int(hdr[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}

	needAuth := s.creds != nil
	if needAuth {
		// 选择 0x02 (username/password)
		if !containsByte(methods, 0x02) {
			_, _ = c.Write([]byte{0x05, 0xFF})
			return errors.New("no acceptable auth method")
		}
		if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
			return err
		}
		// RFC 1929 子协商
		ver, err := c.ReadByte()
		if err != nil {
			return err
		}
		ulen, err := c.ReadByte()
		if err != nil {
			return err
		}
		uname := make([]byte, ulen)
		if _, err := io.ReadFull(c, uname); err != nil {
			return err
		}
		plen, err := c.ReadByte()
		if err != nil {
			return err
		}
		pass := make([]byte, plen)
		if _, err := io.ReadFull(c, pass); err != nil {
			return err
		}
		if ver != 0x01 || !s.creds(string(uname), string(pass)) {
			_, _ = c.Write([]byte{0x01, 0x01})
			return errors.New("auth failed")
		}
		_, _ = c.Write([]byte{0x01, 0x00})
		return nil
	}

	// 无认证：选择 0x00
	if !containsByte(methods, 0x00) {
		_, _ = c.Write([]byte{0x05, 0xFF})
		return errors.New("no acceptable auth method")
	}
	_, err := c.Write([]byte{0x05, 0x00})
	return err
}

// handleRequest 处理 CONNECT 请求并建立隧道。
func (s *SOCKSServer) handleRequest(ctx context.Context, c *bufConn) error {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return errors.New("bad socks version")
	}
	cmd := hdr[1]
	atyp := hdr[3]

	host, err := readAddr(c, atyp)
	if err != nil {
		return err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(c, portBytes); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portBytes)
	dest := net.JoinHostPort(host, strconv.Itoa(int(port)))

	if cmd != 0x01 { // 仅支持 CONNECT
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return errors.New("only CONNECT supported")
	}

	up, err := s.dial(ctx, dest)
	if err != nil {
		// REP 0x05 (connection refused)
		_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer up.Close()

	// 成功响应：绑定地址用 0.0.0.0:0
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}

	// 双向隧道
	go func() {
		_, _ = io.Copy(up, c)
		up.Close()
	}()
	_, _ = io.Copy(c, up)
	return nil
}

func (s *SOCKSServer) dial(ctx context.Context, dest string) (net.Conn, error) {
	if s.Dial != nil {
		return s.Dial(ctx, "tcp", dest)
	}
	if s.backend == nil {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", dest)
	}
	switch s.backend.Kind {
	case "http":
		return dialViaHTTPProxy(ctx, s.backend, dest)
	case "socks5":
		return dialSocks5(ctx, s.backend.Addr, s.backend.User, s.backend.Pass, dest)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", dest)
}

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}

// readAddr 按 ATYP 读取目标地址。
func readAddr(c *bufConn, atyp byte) (string, error) {
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03: // 域名
		l, err := c.ReadByte()
		if err != nil {
			return "", err
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	}
	return "", fmt.Errorf("unsupported ATYP %d", atyp)
}

// bufConn 包装 net.Conn + bufio.Reader，便于读取整块。
type bufConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufConn(c net.Conn) *bufConn {
	return &bufConn{Conn: c, reader: bufio.NewReader(c)}
}

func (c *bufConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *bufConn) ReadByte() (byte, error) {
	return c.reader.ReadByte()
}

// dialSocks5 作为 SOCKS5 客户端连接到目标（用于链经 socks5 后端）。
func dialSocks5(ctx context.Context, serverAddr, user, pass, target string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("dial socks5 server %s: %w", serverAddr, err)
	}
	br := newBufConn(conn)

	// 握手：优先 username/password，其次 no-auth
	methods := []byte{0x00}
	if user != "" {
		methods = []byte{0x02, 0x00}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(br, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 {
		conn.Close()
		return nil, errors.New("bad socks5 greeting response")
	}
	switch resp[1] {
	case 0x00:
		// no auth ok
	case 0x02:
		if user == "" {
			conn.Close()
			return nil, errors.New("server requires auth")
		}
		if _, err := conn.Write(buildUserPass(user, pass)); err != nil {
			conn.Close()
			return nil, err
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(br, authResp); err != nil {
			conn.Close()
			return nil, err
		}
		if authResp[1] != 0x00 {
			conn.Close()
			return nil, errors.New("socks5 auth failed")
		}
	default:
		conn.Close()
		return nil, errors.New("no acceptable socks5 auth method")
	}

	// CONNECT 请求
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req := buildConnectRequest(host, uint16(port))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	// 响应头
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(br, hdr); err != nil {
		conn.Close()
		return nil, err
	}
	if hdr[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed with rep 0x%02x", hdr[1])
	}
	// 读取绑定地址
	if _, err := readAddr(br, hdr[3]); err != nil {
		conn.Close()
		return nil, err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		conn.Close()
		return nil, err
	}
	return &bufferedConn{Conn: conn, reader: br.reader}, nil
}

func buildUserPass(user, pass string) []byte {
	b := []byte{0x01, byte(len(user))}
	b = append(b, []byte(user)...)
	b = append(b, byte(len(pass)))
	b = append(b, []byte(pass)...)
	return b
}

// buildConnectRequest 构造 SOCKS5 CONNECT 请求。
func buildConnectRequest(host string, port uint16) []byte {
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		// 域名
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port))
	return req
}
