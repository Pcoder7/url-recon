package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	flag "github.com/spf13/pflag"
)

// paramCheck represents a job in the pipeline, carrying the URL and a specific parameter to test.
type paramCheck struct {
	url   string
	param string
}

// httpClient is a shared HTTP client configured to ignore TLS certificate errors.
var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: time.Second,
			DualStack: true,
		}).DialContext,
	},
	// Do not follow redirects, as we want to analyze the response of the initial request.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// --- Global Variables for Configuration and State ---

// Delay and Jitter
var (
	minDelay time.Duration
	maxDelay time.Duration
	randSrc  = rand.New(rand.NewSource(time.Now().UnixNano()))
	randMu   sync.Mutex
)

// Debug Mode Flag
var debugMode bool

// User-Agent Rotation
var (
	userAgents []string
	// A default list of diverse User-Agents.
	defaultUserAgents = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Linux; Android 14; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Mobile Safari/537.36",
	}
	minRotate, maxRotate         int64
	requestCounter, nextRotateAt = atomic.Int64{}, atomic.Int64{}
	currentUAIndex               = atomic.Int64{}
	uaMu                         sync.Mutex
)

const (
	// A large, fixed buffer size for channels to prevent pipeline deadlocks.
	pipelineBufferSize = 1000
)

func main() {
	// --- Command-Line Flag Parsing ---
	var concurrency int
	var delayStr, rotateStr, uaFile, inputFile string

	flag.IntVarP(&concurrency, "concurrency", "c", 40, "Number of concurrent workers (1-100)")
	flag.StringVarP(&delayStr, "delay", "d", "0", "Delay per base URL in seconds or range (e.g. '0.1' or '0.1-2.0')")
	flag.StringVar(&rotateStr, "rotate-agent", "0", "Rotate User-Agent every N requests (e.g. '5' or '3-10')")
	flag.StringVar(&uaFile, "ua-file", "", "File path with custom User-Agents (one per line)")
	flag.StringVarP(&inputFile, "file", "f", "", "File path with URLs (reads from stdin if omitted)")
	flag.BoolVar(&debugMode, "debug", false, "Enable debug output for verbose logging (e.g., UA rotations)")
	flag.Parse()

	// --- Configuration Validation and Setup ---
	if concurrency < 1 || concurrency > 100 {
		fmt.Fprintln(os.Stderr, "error: concurrency must be between 1 and 100")
		os.Exit(1)
	}

	var err error
	minDelay, maxDelay, err = parseDelay(delayStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing --delay: %v\n", err)
		os.Exit(1)
	}

	if err = parseRotate(rotateStr); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing --rotate-agent: %v\n", err)
		os.Exit(1)
	}
	if minRotate > 0 {
		setNextRotation()
	}

	loadUAs(uaFile)

	// --- Input Handling ---
	var reader *bufio.Scanner
	if inputFile != "" {
		f, err := os.Open(inputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening file %s: %v\n", inputFile, err)
			os.Exit(1)
		}
		defer f.Close()
		reader = bufio.NewScanner(f)
	} else {
		if stat, _ := os.Stdin.Stat(); (stat.Mode() & os.ModeCharDevice) != 0 {
			fmt.Fprintln(os.Stderr, "Usage: Provide URLs via -f <file> or pipe to stdin")
			flag.PrintDefaults()
			os.Exit(1)
		}
		reader = bufio.NewScanner(os.Stdin)
	}

	// --- Pipeline Construction ---
	in := make(chan paramCheck, pipelineBufferSize)
	stage1 := makePool(in, concurrency, reflectParams)
	stage2 := makePool(stage1, concurrency, verifyAppend)

	// The final stage consumes from stage2. We use a WaitGroup to wait for it to finish.
	var finalWg sync.WaitGroup
	finalWg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer finalWg.Done()
			findXSSChars(stage2)
		}()
	}

	// --- Start Processing ---
	// Goroutine to feed URLs from the input source into the first channel.
	go func() {
		for reader.Scan() {
			in <- paramCheck{url: reader.Text()}
		}
		close(in) // Close the input channel when all URLs are sent.
	}()

	// Wait for the final stage to complete all its work.
	finalWg.Wait()
}

// makePool creates a stage in the pipeline with a specified number of workers.
func makePool(in chan paramCheck, workers int, fn func(paramCheck, chan paramCheck)) chan paramCheck {
	out := make(chan paramCheck, pipelineBufferSize)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for job := range in {
				fn(job, out)
			}
		}()
	}
	// This goroutine waits for all workers in this pool to finish, then closes the output channel.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// --- Pipeline Stages ---

// Stage 1: Takes a URL, applies the rate-limit delay, and finds which parameters are reflected.
func reflectParams(c paramCheck, out chan paramCheck) {
	// CORRECT PLACEMENT: Apply delay here to throttle each worker independently.
	applyDelay()

	keys, _, err := checkReflected(c.url)
	if err != nil {
		return
	}
	for _, k := range keys {
		out <- paramCheck{c.url, k}
	}
}

// Stage 2: Verifies if a simple test string can be appended and reflected.
func verifyAppend(c paramCheck, out chan paramCheck) {
	ok, err := checkAppend(c.url, c.param, "TEST123")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error in append test for %s?%s: %v\n", c.url, c.param, err)
		return
	}
	if ok {
		out <- c
	}
}

// Stage 3: The final stage. Takes verified parameters and tests for common XSS characters.
func findXSSChars(in <-chan paramCheck) {
	for c := range in {
		chars := []string{"\"", "'", "<", ">", "$", "|", "(", ")", "`", ":", ";", "{", "}"}
		var found []string
		for _, ch := range chars {
			ok, err := checkAppend(c.url, c.param, "PREFIX"+ch+"SUFFIX")
			if err == nil && ok {
				found = append(found, ch)
			}
		}
		if len(found) > 0 {
			fmt.Printf("XSS? %s param=%s chars=%v\n", c.url, c.param, found)
		}
	}
}

// --- HTTP Helper Functions ---

// checkReflected performs a GET request and returns any query parameter keys whose values are found in the response body.
func checkReflected(target string) ([]string, string, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", pickUA())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	body := string(bodyBytes)

	// We only care about reflections in HTML content.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "html") {
		return nil, body, nil
	}

	u, _ := url.Parse(target)
	var out []string
	for key, vals := range u.Query() {
		for _, v := range vals {
			if v != "" && strings.Contains(body, v) {
				out = append(out, key)
			}
		}
	}
	return out, body, nil
}

// checkAppend modifies a URL parameter with a suffix and checks if the new value is reflected.
func checkAppend(target, param, suffix string) (bool, error) {
	u, err := url.Parse(target)
	if err != nil {
		return false, err
	}
	q := u.Query()
	orig := q.Get(param)
	q.Set(param, orig+suffix)
	u.RawQuery = q.Encode()

	_, body, err := checkReflected(u.String())
	if err != nil {
		return false, err
	}
	return strings.Contains(body, orig+suffix), nil
}

// --- Configuration Parsers and Handlers ---

// parseDelay parses a delay string into min/max durations. Handles single values ("1.5") and ranges ("1-3").
func parseDelay(s string) (time.Duration, time.Duration, error) {
	if strings.Contains(s, "-") {
		parts := strings.SplitN(s, "-", 2)
		minF, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			return 0, 0, err
		}
		maxF, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return 0, 0, err
		}
		if minF < 0 || maxF < 0 || minF > maxF {
			return 0, 0, fmt.Errorf("invalid delay range")
		}
		return time.Duration(minF * float64(time.Second)), time.Duration(maxF * float64(time.Second)), nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, 0, err
	}
	if f < 0 {
		return 0, 0, fmt.Errorf("negative delay")
	}
	d := time.Duration(f * float64(time.Second))
	return d, d, nil
}

// applyDelay pauses execution for a duration between minDelay and maxDelay.
func applyDelay() {
	if maxDelay == 0 {
		return
	}
	randMu.Lock()
	wait := minDelay
	if maxDelay > minDelay {
		wait += time.Duration(randSrc.Int63n(int64(maxDelay - minDelay)))
	}
	randMu.Unlock()
	time.Sleep(wait)
}

// loadUAs reads User-Agents from a file, falling back to defaults if the file is empty or missing.
func loadUAs(path string) {
	if path != "" {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			s := bufio.NewScanner(f)
			for s.Scan() {
				if u := strings.TrimSpace(s.Text()); u != "" {
					userAgents = append(userAgents, u)
				}
			}
		}
	}
	if len(userAgents) == 0 {
		userAgents = defaultUserAgents
	}
}

// parseRotate parses the agent rotation configuration string ("N" or "N-M").
func parseRotate(s string) error {
	if s == "0" || s == "" {
		minRotate, maxRotate = 0, 0
		return nil
	}
	if strings.Contains(s, "-") {
		parts := strings.SplitN(s, "-", 2)
		lo, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return err
		}
		hi, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return err
		}
		if lo <= 0 || hi < lo {
			return fmt.Errorf("invalid rotate range: %s", s)
		}
		minRotate, maxRotate = lo, hi
	} else {
		x, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || x <= 0 {
			return fmt.Errorf("invalid rotate value: %s", s)
		}
		minRotate, maxRotate = x, x
	}
	return nil
}

// setNextRotation calculates the next request count for UA rotation.
func setNextRotation() {
	randMu.Lock()
	defer randMu.Unlock()
	span := maxRotate - minRotate + 1
	next := minRotate + randSrc.Int63n(span)
	nextRotateAt.Store(requestCounter.Load() + next)
}

// pickUA returns a User-Agent, handling the rotation logic and printing debug messages if enabled.
func pickUA() string {
	if minRotate == 0 {
		return userAgents[0]
	}

	currentCount := requestCounter.Add(1)
	if currentCount >= nextRotateAt.Load() {
		uaMu.Lock()
		defer uaMu.Unlock()

		// Double-check condition after acquiring lock to prevent race conditions.
		if currentCount > nextRotateAt.Load() {
			newIndex := (currentUAIndex.Load() + 1) % int64(len(userAgents))
			currentUAIndex.Store(newIndex)
			setNextRotation()

			// Only print the debug message if the --debug flag was provided.
			if debugMode {
				newUserAgent := userAgents[newIndex]
				fmt.Fprintf(os.Stderr, "[DEBUG] Request #%d: Rotating User-Agent -> %s\n", currentCount, newUserAgent)
			}
		}
	}
	return userAgents[int(currentUAIndex.Load())]
}
