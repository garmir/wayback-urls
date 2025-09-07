package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	waybackAPI = "http://web.archive.org/cdx/search/cdx"
	maxRetries = 3
	maxResponseSize = 100 * 1024 * 1024 // 100MB
)

type Config struct {
	concurrency int
	timeout     time.Duration
	verbose     bool
	subdomain   bool
	unique      bool
	fromDate    string
	toDate      string
	matchType   string
	limit       int
}

var config Config

func init() {
	flag.IntVar(&config.concurrency, "c", 5, "Number of concurrent requests")
	flag.DurationVar(&config.timeout, "t", 30*time.Second, "Request timeout")
	flag.BoolVar(&config.verbose, "v", false, "Verbose output (show errors)")
	flag.BoolVar(&config.subdomain, "s", true, "Include subdomains")
	flag.BoolVar(&config.unique, "u", true, "Only output unique URLs")
	flag.StringVar(&config.fromDate, "from", "", "From date (YYYYMMDD)")
	flag.StringVar(&config.toDate, "to", "", "To date (YYYYMMDD)")
	flag.StringVar(&config.matchType, "match", "prefix", "Match type: exact, prefix, host, domain")
	flag.IntVar(&config.limit, "limit", 0, "Limit results per domain (0 = unlimited)")
}

func main() {
	flag.Parse()

	var domains []string

	if flag.NArg() > 0 {
		// Single domain from command line
		domains = []string{flag.Arg(0)}
	} else {
		// Read domains from stdin
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			domain := strings.TrimSpace(scanner.Text())
			if domain == "" {
				continue
			}
			domains = append(domains, cleanDomain(domain))
		}

		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
			os.Exit(1)
		}
	}

	if len(domains) == 0 {
		fmt.Fprintln(os.Stderr, "No domains provided")
		os.Exit(1)
	}

	// Create HTTP client
	client := &http.Client{
		Timeout: config.timeout,
		Transport: &http.Transport{
			MaxIdleConns:        config.concurrency,
			MaxIdleConnsPerHost: config.concurrency,
			MaxConnsPerHost:     config.concurrency,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		},
	}

	// Process domains
	processDomains(client, domains)
}

func processDomains(client *http.Client, domains []string) {
	jobs := make(chan string, len(domains))
	results := make(chan string, 100)
	
	var wg sync.WaitGroup
	ctx := context.Background()

	// Start workers
	for i := 0; i < config.concurrency; i++ {
		wg.Add(1)
		go worker(ctx, client, jobs, results, &wg)
	}

	// Start result printer
	seen := make(map[string]bool)
	done := make(chan struct{})
	go func() {
		for url := range results {
			if config.unique {
				if seen[url] {
					continue
				}
				seen[url] = true
			}
			fmt.Println(url)
		}
		close(done)
	}()

	// Send jobs
	for _, domain := range domains {
		jobs <- domain
	}
	close(jobs)

	// Wait for workers
	wg.Wait()
	close(results)
	<-done
}

func worker(ctx context.Context, client *http.Client, jobs <-chan string, results chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()

	for domain := range jobs {
		urls, err := fetchWaybackURLs(ctx, client, domain)
		if err != nil {
			if config.verbose {
				fmt.Fprintf(os.Stderr, "Error fetching URLs for %s: %v\n", domain, err)
			}
			continue
		}

		for _, url := range urls {
			results <- url
		}
	}
}

func fetchWaybackURLs(ctx context.Context, client *http.Client, domain string) ([]string, error) {
	// Build query parameters
	params := url.Values{}
	
	// Set URL pattern based on config
	urlPattern := buildURLPattern(domain)
	params.Set("url", urlPattern)
	
	params.Set("output", "json")
	params.Set("fl", "original")
	params.Set("collapse", "urlkey")
	
	// Add date filters if specified
	if config.fromDate != "" {
		params.Set("from", config.fromDate)
	}
	if config.toDate != "" {
		params.Set("to", config.toDate)
	}
	
	// Add match type
	params.Set("matchType", config.matchType)
	
	// Add limit if specified
	if config.limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", config.limit))
	}

	// Build full URL
	fullURL := fmt.Sprintf("%s?%s", waybackAPI, params.Encode())

	// Retry logic
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second * time.Duration(attempt))
		}

		urls, err := doFetch(ctx, client, fullURL)
		if err == nil {
			return urls, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

func doFetch(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", "wayback-urls/2.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Limit response size
	limited := io.LimitReader(resp.Body, maxResponseSize)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// Parse JSON response
	var results [][]string
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	// Extract URLs
	var urls []string
	for i, result := range results {
		// Skip header row
		if i == 0 {
			continue
		}
		
		// Each result should have at least one element (the URL)
		if len(result) > 0 {
			url := result[0]
			// Basic validation
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				urls = append(urls, url)
			}
		}
	}

	return urls, nil
}

func buildURLPattern(domain string) string {
	// Clean domain first
	domain = cleanDomain(domain)
	
	if config.subdomain {
		// Include subdomains
		return fmt.Sprintf("*.%s/*", domain)
	}
	
	// Exact domain only
	return fmt.Sprintf("%s/*", domain)
}

func cleanDomain(domain string) string {
	// Remove protocol
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "ftp://")
	
	// Remove path
	if idx := strings.Index(domain, "/"); idx != -1 {
		domain = domain[:idx]
	}
	
	// Remove port
	if idx := strings.LastIndex(domain, ":"); idx != -1 {
		// Make sure it's not IPv6
		if !strings.Contains(domain[:idx], ":") && !strings.HasPrefix(domain, "[") {
			domain = domain[:idx]
		}
	}
	
	// Remove www. prefix if present
	domain = strings.TrimPrefix(domain, "www.")
	
	// Clean whitespace and lowercase
	domain = strings.TrimSpace(strings.ToLower(domain))
	
	return domain
}