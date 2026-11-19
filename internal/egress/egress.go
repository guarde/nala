package egress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"guarde.sh/nala/internal/auth"
	"guarde.sh/nala/internal/store"
)

const TokenPrefix = "nala_egress_"

const maxLogLines = 100

var hostPatternRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

func ValidHostPattern(p string) bool { return len(p) <= 253 && hostPatternRe.MatchString(p) }

func MatchHost(patterns []string, host string) bool {
	for _, p := range patterns {
		if suffix, ok := strings.CutPrefix(p, "*"); ok {
			if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
		} else if p == host {
			return true
		}
	}
	return false
}

var blockedRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32",
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

func IsPublic(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, r := range blockedRanges {
		if r.Contains(addr) {
			return false
		}
	}
	return true
}

type Proxy struct {
	Store *store.Store
	Log   *slog.Logger

	Lookup       func(ctx context.Context, host string) ([]net.IP, error)
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	AllowPrivate bool
	Ports        map[string]bool

	mu     sync.Mutex
	logged map[string]int
}

func New(st *store.Store, log *slog.Logger) *Proxy {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &Proxy{
		Store: st, Log: log,
		Lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		Dial:   d.DialContext,
		Ports:  map[string]bool{"80": true, "443": true},
		logged: map[string]int{},
	}
}

type denied struct {
	status int
	reason string
}

func (d *denied) Error() string { return d.reason }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	grant, err := p.authenticate(r)
	if err != nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="nala"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}

	target := r.Host
	if r.Method != http.MethodConnect {
		if !r.URL.IsAbs() || r.URL.Scheme != "http" {
			http.Error(w, "this is a forward proxy", http.StatusBadRequest)
			return
		}
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, "80"
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")

	conn, err := p.open(r.Context(), grant, host, port)
	if err != nil {
		var d *denied
		if errors.As(err, &d) {
			p.note(grant, fmt.Sprintf("internet: BLOCKED %s:%s (%s)", host, port, d.reason))
			http.Error(w, "blocked by NALA: "+d.reason, d.status)
			return
		}
		p.note(grant, fmt.Sprintf("internet: could not connect to %s:%s", host, port))
		http.Error(w, "could not connect", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	p.note(grant, fmt.Sprintf("internet: allowed %s:%s", host, port))

	if r.Method == http.MethodConnect {
		p.tunnel(w, conn)
		return
	}
	p.forward(w, r, conn)
}

func (p *Proxy) authenticate(r *http.Request) (store.EgressGrant, error) {
	h, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok {
		return store.EgressGrant{}, errors.New("no credentials")
	}
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return store.EgressGrant{}, err
	}
	_, token, _ := strings.Cut(string(raw), ":")
	if !strings.HasPrefix(token, TokenPrefix) || len(token) > 200 {
		return store.EgressGrant{}, errors.New("bad token")
	}
	return p.Store.EgressGrantByToken(r.Context(), auth.HashToken(token))
}

func (p *Proxy) open(ctx context.Context, g store.EgressGrant, host, port string) (net.Conn, error) {
	if !g.AllowNetwork {
		return nil, &denied{http.StatusForbidden, "internet access is switched off for this organisation"}
	}
	if !p.Ports[port] {
		return nil, &denied{http.StatusForbidden, "only ports 80 and 443 are allowed"}
	}
	if net.ParseIP(host) != nil {
		return nil, &denied{http.StatusForbidden, "connect by hostname, not by IP address"}
	}
	if !MatchHost(g.Patterns, host) {
		return nil, &denied{http.StatusForbidden, "host is not on this organisation's allowed list"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ips, err := p.Lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var usable []net.IP
	for _, ip := range ips {
		if p.AllowPrivate || IsPublic(ip) {
			usable = append(usable, ip)
		}
	}
	if len(usable) == 0 {
		return nil, &denied{http.StatusForbidden, "host resolves to a private address"}
	}
	for _, ip := range usable {
		conn, err := p.Dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("connect failed")
}

func (p *Proxy) tunnel(w http.ResponseWriter, upstream net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tunnelling not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, buf); closeWrite(upstream); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); closeWrite(client); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.CloseWrite()
	} else {
		c.Close()
	}
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstream net.Conn) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	used := false
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if used {
				return nil, errors.New("connection already used")
			}
			used = true
			return upstream, nil
		},
	}
	res, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}

func (p *Proxy) note(g store.EgressGrant, line string) {
	key := fmt.Sprintf("%s/%d", g.JobID, g.Attempt)
	p.mu.Lock()
	if len(p.logged) > 10_000 {
		p.logged = map[string]int{}
	}
	p.logged[key]++
	n := p.logged[key]
	p.mu.Unlock()
	if n > maxLogLines {
		return
	}
	if n == maxLogLines {
		line = "internet: further connection messages suppressed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Store.AppendLogs(ctx, g.JobID, g.Attempt, []store.LogLine{{Stream: "system", Line: line, TS: time.Now()}})
	if err != nil {
		p.Log.Error("write egress log line", "err", err)
	}
}
