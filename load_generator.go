package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// -------------- Config --------------

var (
	serverURL  = flag.String("url", "http://localhost:8080", "Server URL")
	numClients = flag.Int("clients", 20, "Number of concurrent clients")
	testMode   = flag.String("mode", "popular_cpu", "Workload mode: popular_cpu, popular_io, all_miss_cpu, or all_miss_io")
	runTime    = flag.Duration("time", 30*time.Second, "Duration of load test (e.g. 30s, 1m)")
)

// -------------- Popular Text  --------------

var popularTexts []string

const (
	popularTextCount = 20 // Number of repeating items to choose from
	cacheHitRate     = 90 // 90% chance to pick a popular item
)

func init() {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	popularTexts = make([]string, popularTextCount)
	log.Printf("Pre-generating %d popular texts for popular modes...", popularTextCount)
	for i := 0; i < popularTextCount; i++ {
		popularTexts[i] = randomText(r, 150)
	}
}

// -------------- Stats --------------

type Stats struct {
	TotalJobs      int
	SuccessfulJobs int
	FailedJobs     int
	TotalLatency   time.Duration
	mu             sync.Mutex
}

func (s *Stats) Add(latency time.Duration, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalJobs++
	if success {
		s.SuccessfulJobs++
		s.TotalLatency += latency
	} else {
		s.FailedJobs++
	}
}

// -------------- Helpers --------------
var wordList = []string{
	"server", "latency", "throughput", "database", "worker",
	"queue", "cache", "network", "packet", "scheduler",
	"thread", "process", "memory", "disk", "system",
}

func randomText(r *rand.Rand, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(wordList[r.Intn(len(wordList))])
		sb.WriteByte(' ')
	}
	return sb.String()
}

// -------------- Worker Logic --------------

func clientWorker(id int, wg *sync.WaitGroup, stats *Stats, stopCh <-chan struct{}) {
	defer wg.Done()
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
	client := &http.Client{Timeout: 20 * time.Second}

	for {
		select {
		case <-stopCh:
			return
		default:
			start := time.Now()
			success := runOneJob(client, r)
			stats.Add(time.Since(start), success)
		}
	}
}

func runOneJob(client *http.Client, r *rand.Rand) bool {
	var data string
	var serverMode string

	isPopular := false
	if *testMode == "popular_cpu" || *testMode == "popular_io" {
		isPopular = true
	}

	if isPopular {
		// This is a "Get popular" workload (90% hit rate)
		if r.Intn(100) < cacheHitRate {
			data = popularTexts[r.Intn(len(popularTexts))] // 90% chance: pick popular
		} else {
			data = randomText(r, 150) // 10% chance: new item
		}
	}

	switch *testMode {
	case "popular_cpu":
		serverMode = "cpu" // On a miss, the server should do CPU-bound work

	case "popular_io":
		serverMode = "io" // On a miss, the server should do I/O-bound work

	case "all_miss_cpu":
		// 100% cache miss rate.
		data = randomText(r, 300+r.Intn(200)) // 100% unique text
		serverMode = "cpu"                    // On a miss, the server should do CPU-bound work

	case "all_miss_io":
		// 100% cache miss rate.
		data = fmt.Sprintf("IO test block %d", r.Intn(1000000))
		serverMode = "io" // On a miss, the server should do I/O-bound work

	default:
		log.Fatalf("Unknown test mode: %s. Use 'popular_cpu', 'popular_io', 'all_miss_cpu', or 'all_miss_io'", *testMode)
	}

	payload := map[string]string{
		"data": data,
		"mode": serverMode,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", *serverURL+"/upload", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("POST /upload failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("Upload returned %s: %s", resp.Status, string(b))
		return false
	}

	var jobResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		if resp.StatusCode == http.StatusOK {
			return true // This was a cache hit, which is a success
		}
		log.Printf("decode upload: %v", err)
		return false
	}

	if jobResp.Status == "done" {
		return true // cache hit, instant success
	}

	statusURL := fmt.Sprintf("%s/status?job_id=%s", *serverURL, jobResp.ID)
	for i := 0; i < 150; i++ { // up to ~15s total
		sr, err := client.Get(statusURL)
		if err != nil {
			log.Printf("GET /status failed: %v", err)
			return false
		}
		var s struct {
			Status string `json:"status"`
		}
		err = json.NewDecoder(sr.Body).Decode(&s)
		sr.Body.Close()
		if err != nil {
			log.Printf("decode status: %v", err)
			return false
		}

		if s.Status == "done" {
			return true
		}
		if s.Status == "failed" {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false // timeout
}

// -------------- Main --------------

func main() {
	flag.Parse()
	fmt.Println("--- Load Generator ---")
	fmt.Printf("Server URL: %s\nMode: %s\nClients: %d\nDuration: %s\n",
		*serverURL, *testMode, *numClients, runTime.String())

	var wg sync.WaitGroup
	stats := &Stats{}
	stopCh := make(chan struct{})

	for i := 0; i < *numClients; i++ {
		wg.Add(1)
		go clientWorker(i, &wg, stats, stopCh)
	}
	time.Sleep(*runTime)
	close(stopCh)

	// Wait for all workers to finish
	wg.Wait()
	if resp, err := http.Get(*serverURL + "/separator"); err == nil {
		resp.Body.Close()
	}
	// --- Summary ---
	fmt.Println("\n--- Test Results ---")
	fmt.Printf("Total Jobs: %d\nSuccessful: %d\nFailed: %d\n",
		stats.TotalJobs, stats.SuccessfulJobs, stats.FailedJobs)

	if stats.SuccessfulJobs > 0 {
		throughput := float64(stats.SuccessfulJobs) / runTime.Seconds()
		avgLatency := stats.TotalLatency.Seconds() / float64(stats.SuccessfulJobs)
		fmt.Printf("Average Throughput: %.2f jobs/sec\n", throughput)
		fmt.Printf("Average Latency: %.3f sec/job\n", avgLatency)
	}
	fmt.Println("----------------------")
}
